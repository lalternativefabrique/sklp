package controller

import (
	"sync"
	"testing"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

// countingGateway records EnsureUp / Down calls so the lifecycle behaviour is
// observable.
type countingGateway struct {
	mu        sync.Mutex
	ups, down int
}

func (g *countingGateway) Route(string, string, []spacemanager.Domain) error { return nil }
func (g *countingGateway) Unroute(string, string) error                      { return nil }
func (g *countingGateway) PruneRoutes(map[spacemanager.RouteKey]bool) error  { return nil }
func (g *countingGateway) EnsureUp() error                                   { g.mu.Lock(); g.ups++; g.mu.Unlock(); return nil }
func (g *countingGateway) Down() error                                       { g.mu.Lock(); g.down++; g.mu.Unlock(); return nil }
func (g *countingGateway) Status() (spacemanager.GatewayStatus, error) {
	return spacemanager.GatewayStatus{Driver: "counting", Running: true}, nil
}

// TestGatewayAcquireSharesSingleton: the host gateway shared by N spaces in one
// controller is brought up ONCE (first acquire) and reused (same instance) by
// subsequent acquires — the per-process memoisation. EnsureUp is idempotent at
// the driver level too, so a redundant call would be harmless, but the manager
// avoids it.
func TestGatewayAcquireSharesSingleton(t *testing.T) {
	gw := &countingGateway{}
	spacemanager.RegisterGatewayDriver("counting-share", func() (spacemanager.GatewayDriver, error) { return gw, nil })

	m := newGatewayManager()

	g1, err := m.acquire("counting-share")
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	g2, err := m.acquire("counting-share")
	if err != nil {
		t.Fatalf("acquire 2: %v", err)
	}
	if g1 != g2 {
		t.Error("two acquires of one driver must return the SAME gateway instance")
	}
	if gw.ups != 1 {
		t.Errorf("EnsureUp called %d times, want exactly 1 (shared singleton)", gw.ups)
	}
}

// TestGatewayNeverAutoDown is the core invariant of the host-singleton model:
// the manager NEVER calls Down. The gateway is a host-level singleton shared
// across detached per-space supervisor processes, so a space teardown — which an
// in-process ref count would have read as "the last release" — must not stop the
// front door the OTHER live spaces (in this and other processes) depend on. Only
// an explicit `sklp gateway down` tears it down.
func TestGatewayNeverAutoDown(t *testing.T) {
	gw := &countingGateway{}
	spacemanager.RegisterGatewayDriver("counting-nodown", func() (spacemanager.GatewayDriver, error) { return gw, nil })

	m := newGatewayManager()
	if _, err := m.acquire("counting-nodown"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// The manager exposes no release/Down path: the only way down is the driver's
	// own Down(), reserved for `sklp gateway down`. Nothing the manager does here
	// may have called it.
	if gw.down != 0 {
		t.Errorf("Down called %d times, want 0 — the gateway is a host singleton the manager never stops", gw.down)
	}
}
