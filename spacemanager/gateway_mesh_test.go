package spacemanager

import (
	"testing"
	"time"
)

// meshOptsFor wires a fake gateway + a static mesh resolver into agent options.
func meshOptsFor(gw Gateway, resolve func(space, service string) (string, bool), routes ...ResolvedRoute) Options {
	o := gatewayOpts(gw, "front", routes...)
	o.MeshResolver = resolve
	return o
}

// TestGateway_MeshRouteResolvesToPeer: a route targeting "<space>/<service>"
// is declared with the hosting node's mesh IP on :80 as its sole backend and
// the internal name as BackendHost.
func TestGateway_MeshRouteResolvesToPeer(t *testing.T) {
	gw := newFakeGateway()
	resolve := func(space, service string) (string, bool) {
		if space == "alpha" && service == "web" {
			return "10.90.0.7", true
		}
		return "", false
	}
	desired := []Service{{Name: "local", Command: []string{"sh", "-c", "sleep 30"}}}
	a, cancel, wg := startNewAgent(t, newCounting(), nil, desired,
		meshOptsFor(gw, resolve, ResolvedRoute{Service: "alpha/web", Host: "b.example.com"}))
	defer wg.Wait()
	defer cancel()

	a.RefreshMeshRoutes()
	waitFor(t, 2*time.Second, "mesh route declared", func() bool { return gw.routeCount("b.example.com") >= 1 })
	d, _ := gw.lastRoute("b.example.com")
	if len(d.Backends) != 1 || d.Backends[0] != "10.90.0.7:80" {
		t.Fatalf("backends = %v, want [10.90.0.7:80]", d.Backends)
	}
	if d.BackendHost != "web.alpha.internal" {
		t.Fatalf("BackendHost = %q", d.BackendHost)
	}

	// Same peer set again: change-detected, no re-declaration.
	before := gw.routeCount("b.example.com")
	a.RefreshMeshRoutes()
	if gw.routeCount("b.example.com") != before {
		t.Fatal("unchanged peer set re-declared the route")
	}
}

// TestGateway_MeshRouteUnresolvedUnroutes: a target the peer cache cannot
// resolve is unrouted rather than left pointing at a stale backend, and comes
// back once the peer appears.
func TestGateway_MeshRouteUnresolvedUnroutes(t *testing.T) {
	gw := newFakeGateway()
	ip := ""
	resolve := func(space, service string) (string, bool) { return ip, ip != "" }
	desired := []Service{{Name: "local", Command: []string{"sh", "-c", "sleep 30"}}}
	a, cancel, wg := startNewAgent(t, newCounting(), nil, desired,
		meshOptsFor(gw, resolve, ResolvedRoute{Service: "alpha/web", Host: "b.example.com"}))
	defer wg.Wait()
	defer cancel()

	a.RefreshMeshRoutes()
	if gw.routeCount("b.example.com") != 0 {
		t.Fatal("unresolved mesh target was routed")
	}

	ip = "10.90.0.9"
	a.RefreshMeshRoutes()
	waitFor(t, 2*time.Second, "route appears once the peer does", func() bool { return gw.routeCount("b.example.com") >= 1 })

	// The peer vanishes: the route is dropped.
	ip = ""
	a.RefreshMeshRoutes()
	waitFor(t, 2*time.Second, "route dropped with the peer", func() bool { return gw.unrouteCount("front/alpha/web") >= 1 })
}

// TestGateway_MeshRouteRedeclaredWhenRoutesChange: a re-apply that changes a
// mesh target's routes is declared even though the hosting node's IP did not
// move. Deduplicating on the backend alone left the front door serving the
// route set the target had when its peer was first resolved.
func TestGateway_MeshRouteRedeclaredWhenRoutesChange(t *testing.T) {
	gw := newFakeGateway()
	resolve := func(space, service string) (string, bool) { return "10.90.0.7", true }
	desired := []Service{{Name: "local", Command: []string{"sh", "-c", "sleep 30"}}}
	a, cancel, wg := startNewAgent(t, newCounting(), nil, desired,
		meshOptsFor(gw, resolve, ResolvedRoute{Service: "alpha/web", Host: "b.example.com"}))
	defer wg.Wait()
	defer cancel()

	a.RefreshMeshRoutes()
	waitFor(t, 2*time.Second, "mesh route declared", func() bool { return gw.routeCount("b.example.com") >= 1 })

	a.SetRoutes([]ResolvedRoute{
		{Service: "alpha/web", Host: "b.example.com"},
		{Service: "alpha/web", Host: "apex.example.com", RedirectApex: true},
	})
	waitFor(t, 2*time.Second, "added route declared", func() bool { return gw.routeCount("apex.example.com") >= 1 })

	d, _ := gw.lastRoute("apex.example.com")
	if !d.RedirectApex {
		t.Fatal("added route lost RedirectApex")
	}
	if d.BackendHost != "web.alpha.internal" {
		t.Fatalf("BackendHost = %q", d.BackendHost)
	}
}

// TestValidateGateway_MeshTargets: "<space>/<service>" targets skip local
// service validation; malformed ones are refused.
func TestValidateGateway_MeshTargets(t *testing.T) {
	cfg := func(target string) *DeployConfig {
		return &DeployConfig{
			Services: map[string]ServiceSpec{"web": {Image: "x", Port: 80}},
			Spaces: map[string]SpaceSpec{
				"front": {Gateway: &SpaceGatewaySpec{Routes: []RouteSpec{{Service: target, Host: "b.example.com"}}}},
			},
		}
	}
	if err := cfg("alpha/web").validateGateway(); err != nil {
		t.Fatalf("valid mesh target refused: %v", err)
	}
	for _, bad := range []string{"/web", "alpha/", "a/b/c"} {
		if err := cfg(bad).validateGateway(); err == nil {
			t.Fatalf("malformed mesh target %q accepted", bad)
		}
	}
}
