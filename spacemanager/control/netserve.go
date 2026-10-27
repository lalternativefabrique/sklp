package control

// netserve.go adds the NETWORK control listener: the SAME mux Serve mounts on the
// local Unix socket, served additionally on a TCP+TLS listener and gated by a
// Bearer-token middleware. This is the kube-apiserver / Talos remote path — the
// supervisor exposes its control API on a port so a space can be piloted from
// another machine, with no SSH.
//
// Trust boundary: the LOCAL Unix socket (Serve) stays auth-free (a local caller
// is trusted); the NETWORK listener REQUIRES a valid token on every request — no
// anonymous network access. TLS uses the per-space self-signed cert; the client
// pins its fingerprint (or opts into --insecure on a LAN).

import (
	"context"
	"crypto/tls"
	"net/http"
	"strings"
)

// ServeNet runs the control API for a SINGLE space on a TCP+TLS listener at
// addr (e.g. ":9443"), behind a token-auth middleware, until ctx is
// cancelled. It is the single-space degenerate case of ServeNetMulti: a
// Registry with exactly this one (space, sup) entry, so there is only one
// dispatch implementation (newMultiMux) to reason about — a client that never
// calls WithSpace still reaches this space at its BARE route (e.g.
// "/v1/apply"), because a single-entry Registry accepts an unprefixed
// request the same way newMux always did (see newMultiMux's root handler:
// RouteSpaces is the only reserved prefix, everything else falls through to
// the sole registered space when there is exactly one). shutdown is the
// supervisor's self-stop hook (POST /v1/down); may be nil.
//
// A bind failure (port in use) is returned to the caller, which logs it as a
// WARNING and keeps the local socket serving — the network listener is opt-in and
// best-effort, never fatal to the deploy.
func ServeNet(ctx context.Context, space string, sup Supervisor, addr string, cert tls.Certificate, token string, shutdown func()) error {
	reg := NewRegistry()
	reg.Register(space, sup, shutdown)
	return ServeNetMulti(ctx, reg, addr, cert, token, nil)
}

// tokenAuth wraps h so every request must carry `Authorization: Bearer <token>`
// matching the expected token; otherwise it replies 401 and does NOT call h. The
// comparison is constant-time (subtle.ConstantTimeCompare) so a wrong token can
// not be guessed byte-by-byte via timing. The /v1/exec hijack passes the same
// header on its raw request line, so it is authenticated here too before the
// takeover.
func tokenAuth(token string, h http.Handler) http.Handler {
	return tokenAuthSource(newTokenSource("", token), h)
}

// bearerToken extracts the token from an `Authorization: Bearer <token>` header
// value (case-insensitive scheme), returning "" when absent/malformed.
func bearerToken(authz string) string {
	const prefix = "bearer "
	if len(authz) < len(prefix) || !strings.EqualFold(authz[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(authz[len(prefix):])
}
