//go:build linux

package mux

import (
	"github.com/lalternativefabrique/sklp/spacemanager"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
	"strings"
)

// Target is where the mux sends a matched host: a service in a space, on a port.
// The mux fd-passes the connection to that space's worker asking it to dial
// Service:Port in the netns (resolved by space DNS) — so for the common case the
// connection reaches the service DIRECTLY, with no per-space ingress container in
// the path. An L7 route (path/TLS/middleware) instead targets the injected
// ingress service; the aggregation chooses which.
type Target struct {
	Space   string
	Service string
	Port    int
}

// HostTargets builds the mux routing table from every live space's persisted
// manifest: each gateway route contributes host -> Target. The port is joined
// from the route's service in the SAME manifest (ResolvedRoute carries no port).
//
// Routing choice (option 4): a plain Host route (no path) targets the service
// DIRECTLY — no ingress container needed. A route with a Path needs L7
// path-routing, so it targets the injected ingress service instead (the mux
// can't split by path itself). A space whose manifest cannot be read is skipped.
func HostTargets() (map[string]Target, error) {
	spaces, err := control.ListSpaces()
	if err != nil {
		return nil, err
	}
	out := map[string]Target{}
	for _, name := range spaces {
		services, meta, err := control.LoadManifest(name)
		if err != nil {
			continue
		}
		portOf := make(map[string]int, len(services))
		hasInjectedIngress := false
		for _, s := range services {
			portOf[s.Name] = s.Port
			if s.Name == spacemanager.IngressServiceName {
				hasInjectedIngress = true
			}
		}
		nativeL7 := meta.GatewayMode == spacemanager.GatewayModePerSpace
		addSpaceTargets(out, meta.Name, meta.Routes, portOf, nativeL7, hasInjectedIngress)
	}
	return out, nil
}

// addSpaceTargets folds ONE space's routes into the mux table. Split out of
// HostTargets so the routing choice is a pure function over the manifest data,
// testable without a live space on disk. nativeL7 means the space runs the
// in-worker L7 (per-space mode); hasInjectedIngress means an ingress container
// is in the service set.
func addSpaceTargets(out map[string]Target, space string, routes []spacemanager.ResolvedRoute, portOf map[string]int, nativeL7, hasInjectedIngress bool) {
	l7 := Target{Space: space, Service: spacemanager.L7ServiceName, Port: spacemanager.L7Port}
	injected := Target{Space: space, Service: spacemanager.IngressServiceName, Port: spacemanager.IngressPort}
	for _, r := range routes {
		if r.Host == "" || r.Passthrough > 0 {
			continue
		}
		// The apex redirect answers "www.<host>", which is not a route host of
		// its own: point it at the L7, which resolves it against the apex route
		// and answers the 301. Without this the mux would close the connection
		// before anything could redirect it.
		if r.RedirectApex && nativeL7 && !strings.HasPrefix(r.Host, "www.") {
			out["www."+r.Host] = l7
		}
		// A path route needs an L7 ingress to split by path, and so does any
		// route asking for a behaviour the L7 owns: headers, compression, the
		// per-client-IP rate limit, the per-request timeout and the body cap are
		// all applied there, so a direct service hop would silently skip them.
		// Prefer the native in-worker L7 (per-space mode); else an injected
		// ingress container if present. A plain Host route goes straight to the
		// service (no hop).
		if r.Path != "" || r.SecurityHeaders || r.Compress || r.RedirectApex || r.Rate > 0 || r.Timeout > 0 || r.MaxBody > 0 {
			if nativeL7 {
				out[r.Host] = l7
				continue
			}
			if hasInjectedIngress {
				out[r.Host] = injected
				continue
			}
		}
		if port, ok := portOf[r.Service]; ok {
			out[r.Host] = Target{Space: space, Service: r.Service, Port: port}
			continue
		}
		// Unknown direct port: fall back to an L7 ingress if available.
		if nativeL7 {
			out[r.Host] = l7
		} else if hasInjectedIngress {
			out[r.Host] = injected
		}
	}
}
