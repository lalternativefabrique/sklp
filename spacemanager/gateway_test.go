package spacemanager

import (
	"testing"
)

func TestOpenGatewayFakeDriver(t *testing.T) {
	gw, err := OpenGateway("fake")
	if err != nil {
		t.Fatalf("OpenGateway(fake): %v", err)
	}
	if gw == nil {
		t.Fatal("OpenGateway(fake) returned nil driver")
	}
	if _, ok := gw.(*fakeGateway); !ok {
		t.Fatalf("OpenGateway(fake) = %T, want *fakeGateway", gw)
	}
}

func TestOpenGatewayUnknownDriver(t *testing.T) {
	if _, err := OpenGateway("nope"); err == nil {
		t.Fatal("OpenGateway(nope) = nil error, want unknown-driver error")
	}
}

// TestOpenGatewayDefaultNotLinked: with NO vendor driver blank-imported into the
// deploy test binary, the default ("traefik") name is KNOWN to config validation
// but its constructor is absent here, so OpenGateway returns a clear not-linked
// error rather than constructing a driver. This is the agnosticism invariant in
// action: package deploy carries no traefik constructor.
func TestOpenGatewayDefaultNotLinked(t *testing.T) {
	if _, err := OpenGateway(""); err == nil {
		t.Fatal("OpenGateway(\"\") = nil error, want a not-linked error (traefik not imported into deploy)")
	}
	if _, err := OpenGateway(DefaultGatewayDriver); err == nil {
		t.Fatalf("OpenGateway(%q) = nil error, want a not-linked error", DefaultGatewayDriver)
	}
}

func TestKnownGatewayDriver(t *testing.T) {
	// The default driver name is always known to config validation, even when its
	// constructor lives in an out-of-package driver not imported here.
	if !KnownGatewayDriver("traefik") {
		t.Error("KnownGatewayDriver(traefik) = false, want true")
	}
	if !KnownGatewayDriver("fake") {
		t.Error("KnownGatewayDriver(fake) = false, want true")
	}
	if KnownGatewayDriver("nope") {
		t.Error("KnownGatewayDriver(nope) = true, want false")
	}
}

func TestFakeGatewayRecordsCalls(t *testing.T) {
	f := newFakeGateway()
	if err := f.EnsureUp(); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	d := Domain{Host: "api.example.com", Space: "production", Service: "core", Port: "4000", TLS: true}
	if err := f.Route("production", "core", []Domain{d}); err != nil {
		t.Fatalf("Route: %v", err)
	}
	if err := f.Unroute("production", "core"); err != nil {
		t.Fatalf("Unroute: %v", err)
	}
	if err := f.Down(); err != nil {
		t.Fatalf("Down: %v", err)
	}

	key := fakeRouteKey("production", "core")
	if f.ups != 1 || f.downs != 1 {
		t.Errorf("ups/downs = %d/%d, want 1/1", f.ups, f.downs)
	}
	if len(f.routed) != 1 || f.routed[0].Host != "api.example.com" {
		t.Errorf("routed = %+v, want one record for api.example.com", f.routed)
	}
	if len(f.unrouted) != 1 || f.unrouted[0] != key {
		t.Errorf("unrouted = %v, want [%s]", f.unrouted, key)
	}
	if _, ok := f.routes[key]; ok {
		t.Errorf("route for %s still present after Unroute", key)
	}
	st, _ := f.Status()
	if st.Running {
		t.Error("Status.Running = true after Down")
	}
}

// TestFakeGatewayPruneRoutes: PruneRoutes drops recorded routes whose key is not
// in keep and records each as an unroute — the GC contract asserted on the fake.
func TestFakeGatewayPruneRoutes(t *testing.T) {
	f := newFakeGateway()
	_ = f.Route("prod", "core", []Domain{{Host: "a", Space: "prod", Service: "core"}})
	_ = f.Route("prod", "web", []Domain{{Host: "b", Space: "prod", Service: "web"}})
	_ = f.Route("staging", "core", []Domain{{Host: "c", Space: "staging", Service: "core"}})

	keep := map[RouteKey]bool{
		{Space: "prod", Service: "core"}:    true,
		{Space: "staging", Service: "core"}: true,
	}
	if err := f.PruneRoutes(keep); err != nil {
		t.Fatalf("PruneRoutes: %v", err)
	}

	if _, ok := f.routes[fakeRouteKey("prod", "web")]; ok {
		t.Error("orphan prod/web survived PruneRoutes")
	}
	for _, k := range [][2]string{{"prod", "core"}, {"staging", "core"}} {
		if _, ok := f.routes[fakeRouteKey(k[0], k[1])]; !ok {
			t.Errorf("kept route %s/%s was pruned", k[0], k[1])
		}
	}
	var sawPruned bool
	for _, u := range f.unrouted {
		if u == fakeRouteKey("prod", "web") {
			sawPruned = true
		}
	}
	if !sawPruned {
		t.Errorf("pruned key not recorded in unroute history: %v", f.unrouted)
	}
}
