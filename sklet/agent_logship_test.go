package sklet

import (
	"context"
	"testing"

	"github.com/lalternativefabrique/sklp/sklet/logship"
	"github.com/lalternativefabrique/sklp/spacemanager"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

// TestShipperSet_StopsPreviousOnReRegister guards the leak a re-registered
// space would otherwise cause: control.Registry.Register replaces a handle in
// place, so a second start for the same name must retire the first shipper
// rather than leave it tailing the dead Supervisor's sources.
func TestShipperSet_StopsPreviousOnReRegister(t *testing.T) {
	t.Setenv(logship.EnvIngestKey, "sklp_deadbeef")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")

	s := newShipperSet()
	sup := &stubSupervisor{live: map[string]spacemanager.Service{}}

	s.start(context.Background(), "alpha", "node-1", sup)
	first, ok := s.stops["alpha"]
	if !ok {
		t.Fatal("a configured node must start a shipper for the space")
	}

	s.start(context.Background(), "alpha", "node-1", sup)
	second := s.stops["alpha"]
	if len(s.stops) != 1 {
		t.Fatalf("re-register must not accumulate shippers, got %d", len(s.stops))
	}
	if &first == &second {
		t.Fatal("re-register must replace the tracked shutdown func")
	}

	s.stop("alpha")
	if _, ok := s.stops["alpha"]; ok {
		t.Fatal("stop() must drop the space from the set")
	}
}

// TestShipperSet_StopIsSafeWhenUnconfigured confirms a node that ships nothing
// (no ingest key) tears a space down without panicking on a nil stop func.
func TestShipperSet_StopIsSafeWhenUnconfigured(t *testing.T) {
	t.Setenv(logship.EnvIngestKey, "")
	s := newShipperSet()
	s.start(context.Background(), "alpha", "node-1", nil)
	if len(s.stops) != 0 {
		t.Fatalf("unconfigured node must start no shipper, got %d", len(s.stops))
	}
	s.stop("alpha")
}

// TestAgentSpaceOptions_WiresLogship is the regression guard for the bug this
// fixes: `sklet agent` replaced the per-space StartNetListener, which was the
// only caller of logship.Start, so a control-plane node captured its spaces'
// logs but shipped none. Firing the hooks must move a shipper in and out of the
// set, not merely register the space.
func TestAgentSpaceOptions_WiresLogship(t *testing.T) {
	t.Setenv(logship.EnvIngestKey, "sklp_deadbeef")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")

	reg := control.NewRegistry()
	shippers := newShipperSet()

	o, err := agentSpaceOptions("alpha", "singleton", reg, shippers)
	if err != nil {
		t.Fatalf("agentSpaceOptions: %v", err)
	}
	if o.NetListenerWithDelete == nil || o.OnStopped == nil {
		t.Fatal("NetListenerWithDelete and OnStopped must both be wired")
	}

	sup := &stubSupervisor{live: map[string]spacemanager.Service{}}
	o.NetListenerWithDelete(context.Background(), "alpha", sup, func() {}, func() {}, func() {})

	if _, ok := reg.Supervisor("alpha"); !ok {
		t.Fatal("NetListenerWithDelete must still register the space in the shared registry")
	}
	if _, ok := shippers.stops["alpha"]; !ok {
		t.Fatal("NetListenerWithDelete must start the space's log shipper (logship.Start unwired)")
	}

	o.OnStopped("alpha")
	if _, ok := reg.Supervisor("alpha"); ok {
		t.Fatal("OnStopped must unregister the space")
	}
	if _, ok := shippers.stops["alpha"]; ok {
		t.Fatal("OnStopped must retire the space's log shipper")
	}
}
