package sklet

import (
	"testing"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

// TestAgentGatewayDriverPerSpaceOpensNoDriver: a node booted in per-space mode
// opens NO gateway driver — the front door is the host mux and TLS is
// terminated by the in-worker L7, so standing up traefik would build a front
// door the node never routes through.
func TestAgentGatewayDriverPerSpaceOpensNoDriver(t *testing.T) {
	meta := spacemanager.SpaceMeta{Name: "production", GatewayMode: spacemanager.GatewayModePerSpace}
	if got := agentGatewayDriver(meta); got != "" {
		t.Fatalf("agentGatewayDriver(per-space) = %q, want %q", got, "")
	}
}

// TestAgentGatewayDriverPerSpaceWithRoutesStillOpensNoDriver: routes on the
// manifest do not resurrect the driver in per-space mode. GatewayDriverFor
// returns a driver as soon as a route exists, so the mode must win over it.
func TestAgentGatewayDriverPerSpaceWithRoutesStillOpensNoDriver(t *testing.T) {
	meta := spacemanager.SpaceMeta{
		Name:        "production",
		GatewayMode: spacemanager.GatewayModePerSpace,
		Routes:      []spacemanager.ResolvedRoute{{Service: "core", Host: "app.example.com"}},
	}
	if got := agentGatewayDriver(meta); got != "" {
		t.Fatalf("agentGatewayDriver(per-space + routes) = %q, want %q", got, "")
	}
}

// TestAgentGatewayDriverSingletonKeepsDefault: an empty singleton node still
// arms the default driver up front, so the first routed deploy lands on a
// standing front door rather than a 404.
func TestAgentGatewayDriverSingletonKeepsDefault(t *testing.T) {
	meta := spacemanager.SpaceMeta{Name: "production", GatewayMode: spacemanager.GatewayModeSingleton}
	if got := agentGatewayDriver(meta); got != spacemanager.DefaultGatewayDriver {
		t.Fatalf("agentGatewayDriver(singleton) = %q, want %q", got, spacemanager.DefaultGatewayDriver)
	}
}

// TestResolveGatewayMode_PersistedModeWinsOverFlagDefault reproduces a real
// incident: --gateway-mode defaults to "per-space" whenever the unit doesn't
// pass it explicitly (the common case), so an agent restart on an already
// -deployed singleton node silently flipped its front door to per-space,
// leaving traefik's dynamic dir empty and every request 404ing — despite
// nothing about the deploy having changed.
func TestResolveGatewayMode_PersistedModeWinsOverFlagDefault(t *testing.T) {
	got := resolveGatewayMode(spacemanager.GatewayModeSingleton, spacemanager.GatewayModePerSpace)
	if got != spacemanager.GatewayModeSingleton {
		t.Fatalf("resolveGatewayMode(singleton persisted, per-space flag) = %q, want %q (a restart must not flip an existing deploy's mode)", got, spacemanager.GatewayModeSingleton)
	}
}

// TestResolveGatewayMode_EmptyNodeTakesFlag: a node with no prior deploy (no
// manifest, empty persisted mode) has nothing recorded yet, so the operator's
// CLI flag picks the mode for the first deploy.
func TestResolveGatewayMode_EmptyNodeTakesFlag(t *testing.T) {
	got := resolveGatewayMode("", spacemanager.GatewayModePerSpace)
	if got != spacemanager.GatewayModePerSpace {
		t.Fatalf("resolveGatewayMode(empty persisted, per-space flag) = %q, want %q", got, spacemanager.GatewayModePerSpace)
	}
}
