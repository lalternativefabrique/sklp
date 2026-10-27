package spacemanager

import (
	"testing"
	"time"
)

// routeCount returns how many times Route was called for a host.
func (f *fakeGateway) routeCount(host string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, d := range f.routed {
		if d.Host == host {
			n++
		}
	}
	return n
}

// lastRoute returns the most recent Domain routed for a host (ok=false if none).
func (f *fakeGateway) lastRoute(host string) (Domain, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.routed) - 1; i >= 0; i-- {
		if f.routed[i].Host == host {
			return f.routed[i], true
		}
	}
	return Domain{}, false
}

// unrouteCount returns how many times Unroute was called for a (space/service) key.
func (f *fakeGateway) unrouteCount(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, h := range f.unrouted {
		if h == key {
			n++
		}
	}
	return n
}

// gatewayOpts builds Options wiring a fakeGateway + space name + the resolved
// routes (gateway-backend support comes from the gwBackendLauncher injected
// separately).
func gatewayOpts(gw Gateway, space string, routes ...ResolvedRoute) Options {
	o := testOpts()
	o.Gateway = gw
	o.Space = space
	o.Routes = routes
	return o
}

// TestGateway_RouteOnReady: a domained service reaching Ready triggers
// fakeGateway.Route with the resolved Host/Service/Port/Space and the loopback
// backend from the gateway-backend launcher. A non-domained service routes nothing.
func TestGateway_RouteOnReady(t *testing.T) {
	l := newGWBackendLauncher()
	gw := newFakeGateway()
	desired := []Service{
		{Name: "core", Command: []string{"sh", "-c", "sleep 30"}, Port: 4000},
		{Name: "worker", Command: []string{"sh", "-c", "sleep 30"}, Port: 9000}, // no route
	}
	a := NewAgent(l, nil, desired, gatewayOpts(gw, "production",
		ResolvedRoute{Service: "core", Host: "api.example.com"}))
	cancel, wg := startAgent(t, a)
	defer wg.Wait()
	defer cancel()

	waitFor(t, 3*time.Second, "routed service routed", func() bool {
		return gw.routeCount("api.example.com") >= 1
	})

	d, ok := gw.lastRoute("api.example.com")
	if !ok {
		t.Fatal("no route recorded for api.example.com")
	}
	if d.Service != "core" || d.Port != "4000" || d.Space != "production" || !d.TLS {
		t.Fatalf("route = %+v, want Service=core Port=4000 Space=production TLS=true", d)
	}
	if len(d.Backends) != 1 || d.Backends[0] != "127.0.0.1:40001" {
		t.Fatalf("route backends = %v, want [127.0.0.1:40001]", d.Backends)
	}

	// A service with no route is never routed.
	if c := gw.routeCount("worker"); c != 0 {
		t.Fatalf("unrouted service routed %d times, want 0", c)
	}
}

// TestGateway_NeverRoutesEmptyBackends: a route is only ever written with a real
// backend. Before the service is Ready its address doesn't resolve, so writing a
// route then would emit a traefik service with an empty `servers:` list —
// rejected by traefik ("loadBalancer standalone"), dropping the whole file and
// 404ing forever. The agent must Unroute (not Route with []) until Ready. This
// guards the regression: every recorded Route carries at least one backend.
func TestGateway_NeverRoutesEmptyBackends(t *testing.T) {
	l := newGWBackendLauncher()
	gw := newFakeGateway()
	desired := []Service{
		{Name: "core", Command: []string{"sh", "-c", "sleep 30"}, Port: 4000},
	}
	a := NewAgent(l, nil, desired, gatewayOpts(gw, "production",
		ResolvedRoute{Service: "core", Host: "api.example.com"}))
	cancel, wg := startAgent(t, a)
	defer wg.Wait()
	defer cancel()

	waitFor(t, 3*time.Second, "routed once Ready", func() bool {
		return gw.routeCount("api.example.com") >= 1
	})

	// EVERY Route call, at any point in the lifecycle, must have a backend —
	// never an empty servers list.
	gw.mu.Lock()
	defer gw.mu.Unlock()
	for i, d := range gw.routed {
		if len(d.Backends) == 0 {
			t.Fatalf("Route #%d for %q written with EMPTY backends (would 404)", i, d.Host)
		}
	}
}

// TestGateway_RerouteOnBackendChange: when a Ready instance of a domained
// service dies and relaunches, Route is called again so the route's backend set
// stays current.
func TestGateway_RerouteOnBackendChange(t *testing.T) {
	l := newGWBackendLauncher()
	gw := newFakeGateway()
	desired := []Service{
		{Name: "core", Command: []string{"sh", "-c", "sleep 30"}, Port: 4000},
	}
	a := NewAgent(l, nil, desired, gatewayOpts(gw, "production",
		ResolvedRoute{Service: "core", Host: "api.example.com"}))
	cancel, wg := startAgent(t, a)
	defer wg.Wait()
	defer cancel()

	waitFor(t, 3*time.Second, "initial route", func() bool {
		return gw.routeCount("api.example.com") >= 1
	})
	before := gw.routeCount("api.example.com")

	// Kill the live instance; onDeath drops it from routing (re-Route) and a
	// relaunch re-adds it (re-Route again) — at least one more Route call.
	pid := l.pidOf("core-0")
	if pid == 0 {
		t.Fatal("no pid for core-0")
	}
	killPID(t, pid)

	waitFor(t, 3*time.Second, "re-route after backend change", func() bool {
		return gw.routeCount("api.example.com") > before
	})
}

// TestGateway_UnrouteOnRemove: RemoveService on a routed service calls
// Unroute(space/service) exactly once; an unrouted service triggers no Unroute.
func TestGateway_UnrouteOnRemove(t *testing.T) {
	l := newGWBackendLauncher()
	gw := newFakeGateway()
	desired := []Service{
		{Name: "core", Command: []string{"sh", "-c", "sleep 30"}, Port: 4000},
		{Name: "worker", Command: []string{"sh", "-c", "sleep 30"}, Port: 9000},
	}
	a := NewAgent(l, nil, desired, gatewayOpts(gw, "production",
		ResolvedRoute{Service: "core", Host: "api.example.com"}))
	cancel, wg := startAgent(t, a)
	defer wg.Wait()
	defer cancel()

	waitFor(t, 3*time.Second, "routed service routed", func() bool {
		return gw.routeCount("api.example.com") >= 1
	})

	a.RemoveService("worker")
	a.RemoveService("core")

	coreKey := fakeRouteKey("production", "core")
	waitFor(t, 3*time.Second, "routed service unrouted", func() bool {
		return gw.unrouteCount(coreKey) == 1
	})
	if c := gw.unrouteCount(coreKey); c != 1 {
		t.Fatalf("Unroute(%s) called %d times, want 1", coreKey, c)
	}
	// worker has no route: it was never unrouted.
	if c := gw.unrouteCount(fakeRouteKey("production", "worker")); c != 0 {
		t.Fatalf("unrouted service unrouted %d times, want 0", c)
	}
}

// TestGateway_NilGatewayNoop: with no Gateway injected, a routed service
// reaching Ready does not panic and nothing happens gateway-side.
func TestGateway_NilGatewayNoop(t *testing.T) {
	l := newGWBackendLauncher()
	desired := []Service{
		{Name: "core", Command: []string{"sh", "-c", "sleep 30"}, Port: 4000},
	}
	opts := testOpts()
	opts.Space = "production" // no Gateway
	opts.Routes = []ResolvedRoute{{Service: "core", Host: "api.example.com"}}
	a := NewAgent(l, nil, desired, opts)
	cancel, wg := startAgent(t, a)
	defer wg.Wait()
	defer cancel()

	waitFor(t, 3*time.Second, "service up", func() bool {
		return a.State("core") == Ready
	})
	// No panic, no gateway state to assert — reaching here is the assertion.
}
