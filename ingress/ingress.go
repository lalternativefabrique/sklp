// Package ingress is sklp's native L7 ingress: the in-space reverse proxy that
// replaces traefik for the common case. It routes an HTTP request by Host (and
// optional path prefix) to a backend addressed by SPACE DNS NAME (service:port),
// the same backend shape the traefik driver emits. It runs INSIDE the space
// netns, so "nginx:80" resolves via the space resolver and reaches the container
// directly — no host port, no vendor binary.
//
// This file is the routing + proxying core, pure net/http, unit-testable without
// a netns. TLS termination (crypto/tls + a cert source) layers on top later.
package ingress

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// GatewayService and GatewayPort are the reserved space-DNS name and in-netns
// port of this ingress's relayed listener, the one wrapped in Relayed. The
// node's Network Controller registers them like a service, so a front can
// hand a whole public host to the space and let this handler pick the backend
// from the space's own routes (ADR-021).
const (
	GatewayService = "sklp-l7-relay"
	GatewayPort    = 8002
)

// Route is one ingress rule: a Host (required), an optional Path prefix, and the
// backend to proxy to (a DNS name:port resolved in the space netns). It is the
// neutral shape the driver renders and the proxy consumes — no vendor config.
type Route struct {
	Host    string `json:"host"`
	Path    string `json:"path,omitempty"` // "" = host root
	Backend string `json:"backend"`        // e.g. "nginx:80" (space DNS name:port)

	// Rate, when set (RPS > 0), rate-limits this route per client IP. Zero =
	// unlimited. Keyed into the limiter table by (Host, Path) so a config reload
	// re-uses the live buckets for an unchanged route.
	Rate RateSpec `json:"rate,omitempty"`

	// RedirectApex answers a "www." host with a 301 to the same URL on the bare
	// host. Declared on the apex route, since the redirect belongs to the domain.
	RedirectApex bool `json:"redirect_apex,omitempty"`

	// SecurityHeaders adds HSTS, X-Frame-Options, X-Content-Type-Options and a
	// referrer policy to the response.
	SecurityHeaders bool `json:"security_headers,omitempty"`

	// Compress gzips text responses when the client accepts it.
	Compress bool `json:"compress,omitempty"`

	// MaxBody, when > 0, caps ONE request's body in bytes; a client exceeding it
	// gets 413. Zero = unbounded, which is what an upload route needs. Enforced
	// with http.MaxBytesReader, so the body is cut WHILE being read rather than
	// buffered first — unlike traefik's buffering middleware, which tampons the
	// body before forwarding it.
	MaxBody int64 `json:"max_body,omitempty"`

	// Timeout, when > 0, bounds ONE request end to end: the backend dial, the
	// response, and the body transfer. Zero = unbounded, which is what a route
	// serving large downloads or a WebSocket needs. It is enforced through the
	// request CONTEXT, not http.TimeoutHandler: that wrapper supports neither
	// Hijacker nor Flusher, so it would break exactly the upgrades and streams
	// a per-route opt-in is meant to leave alone.
	Timeout time.Duration `json:"timeout,omitempty"`
}

// key uniquely identifies a route by its match tuple, for limiter reuse across
// SetRoutes calls.
func (r Route) key() string { return r.Host + "\x00" + r.Path }

// Handler is an http.Handler that routes by Host + longest-matching Path prefix.
// Routes are swappable at runtime (SetRoutes) so a config reload needs no
// restart — the in-process analogue of traefik's file-provider watch.
type Handler struct {
	mu     sync.RWMutex
	routes []Route
	// proxies caches one reverse proxy per backend so we don't rebuild it per
	// request. Keyed by backend addr.
	proxies map[string]*httputil.ReverseProxy

	// limiters holds one rate limiter per rate-limited route, keyed by Route.key()
	// so an unchanged route keeps its live per-IP buckets across a config reload.
	limiters map[string]*rateLimiter

	// dial, when non-nil, overrides how the reverse proxy connects to a backend.
	// The in-worker ingress sets it to resolve a service name via the space
	// nameTable (in memory) instead of Go's host /etc/resolv.conf resolver, which
	// would not see the space DNS. nil = the default net dialer.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// New builds an empty handler. Load routes with SetRoutes.
func New() *Handler {
	return &Handler{
		proxies:  map[string]*httputil.ReverseProxy{},
		limiters: map[string]*rateLimiter{},
	}
}

// WithDialer sets a custom backend dialer (DialContext shape). The in-worker
// ingress uses it to resolve service names via the space nameTable directly,
// bypassing the host resolver. Must be called before serving.
func (h *Handler) WithDialer(dial func(ctx context.Context, network, addr string) (net.Conn, error)) *Handler {
	h.dial = dial
	return h
}

// SetRoutes replaces the routing table wholesale (the desired set). Hosts are
// lower-cased; routes are sorted by descending path length so the longest prefix
// wins at match time. Stale backend proxies are dropped.
func (h *Handler) SetRoutes(routes []Route) {
	norm := make([]Route, 0, len(routes))
	live := map[string]bool{}
	for _, r := range routes {
		r.Host = strings.ToLower(r.Host)
		norm = append(norm, r)
		live[r.Backend] = true
	}
	// Longest path first so "/api/v1" is tried before "/api" before "".
	sort.SliceStable(norm, func(i, j int) bool {
		return len(norm[i].Path) > len(norm[j].Path)
	})
	h.mu.Lock()
	h.routes = norm
	for backend := range h.proxies {
		if !live[backend] {
			delete(h.proxies, backend)
		}
	}
	// Reconcile per-route limiters: reuse the live limiter when a route's rate is
	// unchanged (so in-flight per-IP buckets survive a reload), rebuild it when
	// the spec changed, drop it when the route is gone or no longer limited.
	next := make(map[string]*rateLimiter, len(norm))
	for _, r := range norm {
		if !r.Rate.enabled() {
			continue
		}
		k := r.key()
		if cur := h.limiters[k]; cur != nil && cur.spec == r.Rate {
			next[k] = cur
		} else {
			next[k] = newRateLimiter(r.Rate)
		}
	}
	h.limiters = next
	h.mu.Unlock()
}

// ServeHTTP matches the request to a route and proxies it, or returns 404 when
// no route matches the Host+Path (the equivalent of traefik's "no router").
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	origin := originOf(r)
	route, ok := h.match(origin.host, r.URL.Path)
	if !ok {
		// A "www." request matches no route, because redirect_apex is declared
		// on the apex route. Resolve it against the bare host before giving up,
		// so the redirect happens instead of a 404.
		if apex, wasWWW := apexOf(hostOnly(origin.host)); wasWWW {
			if apexRoute, found := h.match(apex, r.URL.Path); found && apexRoute.RedirectApex {
				redirectToApex(w, r, apex, origin.tls)
				return
			}
		}
		http.Error(w, "no route for host", http.StatusNotFound)
		return
	}
	if lim := h.limiterFor(route); !lim.allow(origin.clientIP) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	if route.SecurityHeaders {
		setSecurityHeaders(w.Header(), origin.tls)
	}
	if route.Compress && acceptsGzip(r) {
		gw := &gzipResponseWriter{ResponseWriter: w}
		defer gw.Close()
		w = gw
	}
	if route.MaxBody > 0 && r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, route.MaxBody)
	}
	if route.Timeout > 0 {
		ctx, cancel := context.WithTimeout(r.Context(), route.Timeout)
		defer cancel()
		r = r.WithContext(ctx)
	}
	h.proxyFor(route.Backend).ServeHTTP(w, r)
}

// hostOnly strips the port from a Host header and lower-cases it, matching how
// routes are keyed.
func hostOnly(hostHeader string) string {
	host := strings.ToLower(hostHeader)
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	return host
}

// limiterFor returns the rate limiter for a matched route, or nil (unlimited).
// nil is a valid receiver for allow, so callers need no nil check.
func (h *Handler) limiterFor(route Route) *rateLimiter {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.limiters[route.key()]
}

// match finds the route for host+path: exact host (case-insensitive, port
// stripped) AND the request path under the route's prefix. Routes are pre-sorted
// longest-path-first, so the first hit is the most specific.
func (h *Handler) match(hostHeader, path string) (Route, bool) {
	host := strings.ToLower(hostHeader)
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, r := range h.routes {
		if r.Host != host {
			continue
		}
		if r.Path == "" || pathUnderPrefix(path, r.Path) {
			return r, true
		}
	}
	return Route{}, false
}

// pathUnderPrefix reports whether path is at or under prefix, matching on path
// SEGMENTS so "/api" matches "/api" and "/api/x" but not "/apixyz".
func pathUnderPrefix(path, prefix string) bool {
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := path[len(prefix):]
	return rest == "" || rest[0] == '/' || prefix[len(prefix)-1] == '/'
}

// proxyFor returns the cached reverse proxy for backend, building it on first
// use. The backend is an http://name:port the space DNS resolves in the netns.
func (h *Handler) proxyFor(backend string) *httputil.ReverseProxy {
	h.mu.RLock()
	p := h.proxies[backend]
	h.mu.RUnlock()
	if p != nil {
		return p
	}
	target := &url.URL{Scheme: "http", Host: backend}
	p = httputil.NewSingleHostReverseProxy(target)
	if h.dial != nil {
		// DisableKeepAlives so every request re-dials through h.dial, which
		// round-robins replicas via the space nameTable. With keep-alive the
		// Transport would pin the pooled connection to the FIRST resolved replica
		// and never load-balance. One TCP conn per request is the cost of
		// per-request round-robin without a backend pool.
		p.Transport = &http.Transport{DialContext: h.dial, DisableKeepAlives: true}
	}
	h.mu.Lock()
	if existing := h.proxies[backend]; existing != nil {
		h.mu.Unlock()
		return existing
	}
	h.proxies[backend] = p
	h.mu.Unlock()
	return p
}

// String renders the routing table for logs/debug.
func (h *Handler) String() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var b strings.Builder
	for _, r := range h.routes {
		fmt.Fprintf(&b, "%s%s -> %s\n", r.Host, r.Path, r.Backend)
	}
	return b.String()
}
