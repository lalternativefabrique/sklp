//go:build linux

package mux

import (
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

func targets(routes []spacemanager.ResolvedRoute, nativeL7, injected bool) map[string]Target {
	out := map[string]Target{}
	addSpaceTargets(out, "production", routes, map[string]int{"web": 3000}, nativeL7, injected)
	return out
}

// A rate-limited route must be sent through the L7, which is the only component
// that holds the token buckets. Routed straight to the service, the declared
// limit would be silently unenforced.
func TestAddSpaceTargets_RateLimitedRouteGoesThroughL7(t *testing.T) {
	got := targets([]spacemanager.ResolvedRoute{
		{Service: "web", Host: "app.test", Rate: 10, Burst: 20},
	}, true, false)

	want := Target{Space: "production", Service: spacemanager.L7ServiceName, Port: spacemanager.L7Port}
	if got["app.test"] != want {
		t.Errorf("rate-limited route target = %+v, want the L7 %+v", got["app.test"], want)
	}
}

// Same route without a rate limit (and no other L7-owned behaviour) keeps the
// direct hop — the rate limit must be the only thing that changed.
func TestAddSpaceTargets_PlainRouteStaysDirect(t *testing.T) {
	got := targets([]spacemanager.ResolvedRoute{
		{Service: "web", Host: "app.test"},
	}, true, false)

	want := Target{Space: "production", Service: "web", Port: 3000}
	if got["app.test"] != want {
		t.Errorf("plain route target = %+v, want the service directly %+v", got["app.test"], want)
	}
}

// With no native L7, a rate-limited route falls back to the injected ingress
// container, which runs the same handler.
func TestAddSpaceTargets_RateLimitedFallsBackToInjectedIngress(t *testing.T) {
	got := targets([]spacemanager.ResolvedRoute{
		{Service: "web", Host: "app.test", Rate: 10, Burst: 20},
	}, false, true)

	want := Target{Space: "production", Service: spacemanager.IngressServiceName, Port: spacemanager.IngressPort}
	if got["app.test"] != want {
		t.Errorf("rate-limited route target = %+v, want the injected ingress %+v", got["app.test"], want)
	}
}

// A route whose only L7-owned behaviour is the per-request timeout must still
// go through the L7 — that is where the deadline is applied.
func TestAddSpaceTargets_TimeoutRouteGoesThroughL7(t *testing.T) {
	got := targets([]spacemanager.ResolvedRoute{
		{Service: "web", Host: "app.test", Timeout: 30 * time.Second},
	}, true, false)

	want := Target{Space: "production", Service: spacemanager.L7ServiceName, Port: spacemanager.L7Port}
	if got["app.test"] != want {
		t.Errorf("timeout route target = %+v, want the L7 %+v", got["app.test"], want)
	}
}

// A route whose only L7-owned behaviour is the body cap must still go through
// the L7 — that is where MaxBytesReader is applied.
func TestAddSpaceTargets_MaxBodyRouteGoesThroughL7(t *testing.T) {
	got := targets([]spacemanager.ResolvedRoute{
		{Service: "web", Host: "app.test", MaxBody: 1 << 20},
	}, true, false)

	want := Target{Space: "production", Service: spacemanager.L7ServiceName, Port: spacemanager.L7Port}
	if got["app.test"] != want {
		t.Errorf("max_body route target = %+v, want the L7 %+v", got["app.test"], want)
	}
}
