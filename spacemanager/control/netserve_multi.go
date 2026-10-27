package control

// netserve_multi.go is the multi-space network control API: ONE TCP+TLS
// listener on the node, dispatching to N spaces' Supervisors by path prefix
// (SpaceRoutePrefix). It replaces the single-space assumption baked into
// newMux/ServeNet — see apps/sklp/sklet/agent.go's RunAgent, which now
// materialises every space this node runs, not one fixed at boot.
//
// Design: rather than rewrite every route handler to resolve its Supervisor
// per-request, this reuses newMux(space, sup, shutdown) UNCHANGED, one
// instance per registered space, cached in a per-request-safe map. The root
// handler strips the "/v1/spaces/<space>" prefix and forwards to that space's
// mux, so every existing route (RouteApply, RouteState, RouteLogs, ...) keeps
// its known, tested behaviour verbatim — only the path a client reaches it at
// changes. RouteSpaces (node-scoped, not space-scoped) is answered once at
// the root, exactly as newMux already does for the single-space case.
//
// A single-space node is the degenerate case: ServeNet (unchanged, still
// exported for anything that only ever runs one space) wraps this with a
// Registry holding exactly one entry, so there is only one dispatch
// implementation to reason about, not two.

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Materializer brings up a space this node does not yet run, registering it
// into the Registry the multi-mux dispatches through (typically by calling
// controller.SpaceController.AddSpace) before returning. It blocks until the
// space is ready to serve or has failed to start. Passed to ServeNetMulti so
// a request for an unknown space can be satisfied without restarting the
// process — see apps/sklp/sklet/agent.go's RunAgent, which now discovers
// spaces from persisted manifests only for its INITIAL boot set; a space
// deployed for the first time afterwards reaches this hook instead.
type Materializer func(space string) error

// ServeNetMulti runs the control API for every space in reg on a TCP+TLS
// listener at addr, behind a token-auth middleware, until ctx is cancelled.
// Each space's routes are reached at SpaceRoutePrefix(space)+route (e.g.
// /v1/spaces/production/apply); RouteSpaces answers once at the root,
// unprefixed. materialize, when non-nil, is tried once for a space not (yet)
// in reg before the request 404s (see newMultiMux) — nil keeps the old
// "unknown space is always a 404" behaviour (e.g. ServeNet's single-space
// case, which never materialises new spaces). Mirrors ServeNet's
// bind-failure contract: the caller logs a failed bind as a warning and
// keeps every space's local socket serving regardless.
func ServeNetMulti(ctx context.Context, reg *Registry, addr string, cert tls.Certificate, token string, materialize Materializer) error {
	tcp, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return ServeNetMultiOn(ctx, reg, tcp, cert, token, materialize)
}

// ServeNetMultiOn is ServeNetMulti on an already-open listener, so a caller
// can put a RelayListener in front of the local server.
func ServeNetMultiOn(ctx context.Context, reg *Registry, tcp net.Listener, cert tls.Certificate, token string, materialize Materializer) error {
	tokens := newTokenSource(TokenPath(""), token)
	handler := tokenAuthSource(tokens, newMultiMuxWithTokens(reg, materialize, tokens))
	ln := tls.NewListener(tcp, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})

	srv := newControlServer(handler)
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if serr := srv.Serve(ln); serr != nil && serr != http.ErrServerClosed {
		return serr
	}
	return nil
}

// newMultiMux builds the dispatcher: RouteSpaces at the root (node-scoped,
// same handler shape as newMux's), everything else routed by stripping a
// SpaceRoutePrefix("<name>") prefix and forwarding to that space's cached
// newMux. A per-space mux is built lazily on first request and cached — the
// Supervisor/shutdown pair it closes over never changes for a given
// Register() call, so the cache is safe to keep for the handle's lifetime;
// Unregister invalidates it.
//
// A request for a space reg does not (yet) know about is not an automatic
// 404: when materialize is non-nil it is tried once (deduplicated across
// concurrent requests for the same space via singleflight, so two racing
// deploys of a brand-new space never launch it twice), and reg is
// re-consulted afterwards. Only a still-missing space, or materialize
// declining/failing, 404s.
func newMultiMux(reg *Registry, materialize Materializer) http.Handler {
	return newMultiMuxWithTokens(reg, materialize, nil)
}

// newMultiMuxWithTokens is newMultiMux with the listener's token source, which
// RouteTokenRotate rotates in place; nil disables that route.
func newMultiMuxWithTokens(reg *Registry, materialize Materializer, tokens *tokenSource) http.Handler {
	spaceMuxes := &muxCache{muxes: map[string]cachedMux{}}
	var group singleflight.Group

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == RouteSpaces {
			serveSpacesInventory(w, r)
			return
		}
		if r.URL.Path == RouteRestores {
			serveRestores(w, r)
			return
		}
		if r.URL.Path == RoutePublicNIC {
			servePublicNIC(w, r)
			return
		}
		// Node-scoped gateway control, like the inventory: the singleton fronts
		// every space, so neither route carries a space prefix. Exact matches
		// only — the space-scoped RouteGatewayRoutes keeps its own path.
		if r.URL.Path == RouteGateway {
			serveGatewayStatus(w, r)
			return
		}
		if r.URL.Path == RouteGatewayUp {
			serveGatewayUp(w, r)
			return
		}
		if r.URL.Path == RouteGatewayCerts {
			serveGatewayCerts(w, r)
			return
		}
		if r.URL.Path == RouteTokenRotate {
			serveTokenRotate(w, r, tokens)
			return
		}
		if r.URL.Path == RouteMeshPeers || strings.HasPrefix(r.URL.Path, RouteMeshPeers+"/") {
			serveMeshPeers(w, r)
			return
		}
		if space, rest, ok := splitSpacePath(r.URL.Path); ok {
			if err := validSpaceName(space); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			// The manifest lives on disk, not in the supervisor: a space that
			// is stopped (or failed to come up) still HAS a declared shape,
			// and reading it is exactly how an operator finds out why it is
			// not running. Answering it from reg would 404 precisely then.
			if rest == strings.TrimPrefix(RouteManifest, "/v1") {
				serveManifest(w, r, space)
				return
			}
			// Start is the deliberate counterpart of the implicit
			// materialize below: a stopped space is absent from reg, so this
			// has to run BEFORE the lookup that would 404 it.
			if rest == strings.TrimPrefix(RouteStart, "/v1") {
				serveStart(w, r, space, reg, materialize, &group)
				return
			}
			h, ok := reg.get(space)
			// A STOPPED space has no supervisor, so a delete aimed at one used
			// to 404 — leaving the only spaces anyone wants to delete the ones
			// no command could. Materialise it so the teardown has something to
			// tear down, but ONLY when a manifest proves a deploy really
			// created it: a mistyped name has none and still 404s, which is the
			// typo guard below, kept intact.
			if !ok && materialize != nil && rest == strings.TrimPrefix(RouteDown, "/v1") && SpaceIsPersisted(space) {
				if _, err, _ := group.Do(space, func() (any, error) {
					return nil, materialize(space)
				}); err != nil {
					http.Error(w, "space "+space+": "+err.Error(), http.StatusInternalServerError)
					return
				}
				h, ok = reg.get(space)
			}
			// Only materialize for an apply — the one route where "this space
			// doesn't exist yet" is the expected first-deploy case. Every
			// other route (state/logs/exec/restart) targets a space the
			// caller believes ALREADY runs; materializing there means a typo
			// in `sklp deploy delete -space <name>` (or any other verb)
			// silently CREATES an empty space instead of 404ing — found live
			// (a stray "nginx"/"sapcea" space appearing from a mistyped
			// delete, node-d, 2026-08-30).
			if !ok && materialize != nil && rest == strings.TrimPrefix(RouteApply, "/v1") {
				_, err, _ := group.Do(space, func() (any, error) {
					return nil, materialize(space)
				})
				if err != nil {
					http.Error(w, "space "+space+": "+err.Error(), http.StatusInternalServerError)
					return
				}
				h, ok = reg.get(space)
			}
			if !ok {
				http.Error(w, "space "+space+" is not running on this node", http.StatusNotFound)
				return
			}
			spaceMuxes.forSpace(space, h).ServeHTTP(w, stripSpacePrefix(r, rest))
			return
		}
		// No /v1/spaces/<name> prefix: a bare route (e.g. "/v1/apply") only
		// resolves when the node runs EXACTLY one space — the single-space
		// back-compat path ServeNet relies on (a caller that never calls
		// WithSpace still reaches its one space unprefixed). A multi-space node
		// has no unambiguous target for a bare route, so it 404s instead of
		// guessing which space the caller meant. Never materializes: a bare
		// route names no space, so there is nothing to bring up on demand.
		names := reg.Names()
		if len(names) != 1 {
			http.NotFound(w, r)
			return
		}
		h, _ := reg.get(names[0])
		spaceMuxes.forSpace(names[0], h).ServeHTTP(w, r)
	})
}

// serveSpacesInventory answers GET /v1/spaces: the node's whole space
// inventory, independent of which (if any) individual space a request also
// names. Node-scoped, never dispatched per-space.
func serveSpacesInventory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "spaces: GET required", http.StatusMethodNotAllowed)
		return
	}
	inv := currentInventory()
	if inv == nil {
		http.Error(w, "spaces: this node serves no inventory", http.StatusNotImplemented)
		return
	}
	spaces, err := inv.SpacesJSON(r.Context())
	if err != nil {
		http.Error(w, "spaces: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, SpacesResponse{Node: NodeName(), Spaces: spaces})
}

// splitSpacePath extracts the space name and remaining path from a request
// under RouteSpaces+"/" (e.g. "/v1/spaces/production/apply" ->
// ("production", "/apply", true)). ok is false for a malformed path (no space
// segment) — RouteSpaces itself, with no trailing name, never reaches this
// handler (it is registered separately).
func splitSpacePath(path string) (space, rest string, ok bool) {
	trimmed := strings.TrimPrefix(path, RouteSpaces+"/")
	if trimmed == "" || trimmed == path {
		return "", "", false
	}
	if i := strings.IndexByte(trimmed, '/'); i >= 0 {
		return trimmed[:i], trimmed[i:], true
	}
	// A bare "/v1/spaces/<space>" with nothing after it — no route matches
	// under the space's mux (its shortest real route is e.g. "/v1/health"),
	// but treating it as space="<space>", rest="/" keeps the space lookup
	// (and its 404-vs-unknown-space distinction) uniform with every other case.
	return trimmed, "/", true
}

// stripSpacePrefix returns a shallow clone of r with its URL path rewritten
// to rest (the space-relative path newMux's routes expect, e.g. "/v1/apply"
// — NOT "/apply": newMux's routes are registered as the full RouteX
// constants). r.Clone is used so the original request (and its context) is
// left untouched for any caller holding a reference.
func stripSpacePrefix(r *http.Request, rest string) *http.Request {
	// rest is what followed the space name, e.g. "/apply" for
	// ".../production/apply" — but newMux's routes are the bare RouteX
	// constants ("/v1/apply", "/v1/logs/web", ...), so re-add that root.
	clone := r.Clone(r.Context())
	clone.URL.Path = "/v1" + rest
	clone.URL.RawPath = ""
	return clone
}

// cachedMux pairs a built mux with the Supervisor pointer it was built for,
// so a Register() that REPLACES a space's handle (a redeploy after teardown,
// reusing the same space name) is detected and rebuilt rather than serving
// through a mux still closed over the old, now-dead Supervisor.
type cachedMux struct {
	sup Supervisor
	mux http.Handler
}

// muxCache lazily builds and caches one newMux per space so repeated
// requests do not rebuild the http.ServeMux (and its route table) every time.
// Safe for concurrent use. Keyed by space name but validated against the
// registry's current Supervisor on every lookup (see cachedMux) so a
// same-name Register() after an Unregister() never serves a stale mux.
type muxCache struct {
	mu    sync.Mutex
	muxes map[string]cachedMux
}

func (c *muxCache) forSpace(space string, h spaceHandle) http.Handler {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cm, ok := c.muxes[space]; ok && cm.sup == h.sup {
		return cm.mux
	}
	m := newMuxWithDelete(space, h.sup, h.shutdown, h.purgeShutdown, h.deleteShutdown)
	c.muxes[space] = cachedMux{sup: h.sup, mux: m}
	return m
}

// serveStart answers POST <space>/start: bring a stopped space back up from
// the manifest it kept. A space already registered is left alone and reported
// as such, so pressing Start twice is harmless.
func serveStart(w http.ResponseWriter, r *http.Request, space string, reg *Registry, materialize Materializer, group *singleflight.Group) {
	if r.Method != http.MethodPost {
		http.Error(w, "start: POST required", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := reg.get(space); ok {
		writeJSON(w, StartResponse{Started: false, Space: space})
		return
	}
	if materialize == nil {
		http.Error(w, "start: this node cannot materialise a space", http.StatusNotImplemented)
		return
	}
	if _, err, _ := group.Do(space, func() (any, error) {
		return nil, materialize(space)
	}); err != nil {
		http.Error(w, "space "+space+": "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, ok := reg.get(space); !ok {
		http.Error(w, "space "+space+" did not come up", http.StatusInternalServerError)
		return
	}
	writeJSON(w, StartResponse{Started: true, Space: space})
}

// A client that never reuses nor closes its keep-alive connections (core did,
// one per request) otherwise holds them open forever, here and on the front
// relaying them. No read/write timeout: logs -f and exec stream for hours.
var (
	controlIdleTimeout       = 2 * time.Minute
	controlReadHeaderTimeout = 10 * time.Second
)

func newControlServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		IdleTimeout:       controlIdleTimeout,
		ReadHeaderTimeout: controlReadHeaderTimeout,
	}
}
