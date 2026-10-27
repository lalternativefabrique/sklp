package logship

import (
	"context"
	"testing"
)

// fakeSupervisor is a minimal Supervisor test double keyed by service name.
type fakeSupervisor struct {
	byService map[string][]LogSource
}

func (f fakeSupervisor) DesiredServiceNames() []string {
	names := make([]string, 0, len(f.byService))
	for n := range f.byService {
		names = append(names, n)
	}
	return names
}

func (f fakeSupervisor) ServiceLogSources(name string) []LogSource {
	return f.byService[name]
}

// TestConfigured_RequiresEnv confirms the no-op gate: without EnvIngestKey set,
// a node must not attempt to ship anything.
func TestConfigured_RequiresEnv(t *testing.T) {
	t.Setenv(EnvIngestKey, "")
	if Configured() {
		t.Fatal("Configured() must be false without an ingest key")
	}
	t.Setenv(EnvIngestKey, "sklp_deadbeef")
	if !Configured() {
		t.Fatal("Configured() must be true with an ingest key set")
	}
}

// TestStart_NoopWithoutIngestKey confirms Start returns a nil shipper and a
// harmless shutdown when logship is not configured — a node need not ship
// logs to run.
func TestStart_NoopWithoutIngestKey(t *testing.T) {
	t.Setenv(EnvIngestKey, "")
	sup := fakeSupervisor{byService: map[string][]LogSource{}}
	s, stop, err := Start(context.Background(), "production", "node-1", sup)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if s != nil {
		t.Fatal("want a nil Shipper when not configured")
	}
	stop() // must not panic
}

// TestShipper_ReconcileTracksDesiredServices confirms reconcile() derives the
// tailer's target set from EVERY desired service's log sources, not just one.
func TestShipper_ReconcileTracksDesiredServices(t *testing.T) {
	var got []taggedSource
	tl := newTailer(func(string, string) {})
	sup := fakeSupervisor{byService: map[string][]LogSource{
		"web": {{Label: "web-0", Path: "/tmp/web.log"}},
		"db":  {{Label: "db-0", Path: "/tmp/db.log"}},
	}}
	s := &Shipper{space: "production", sup: sup, tailer: tl}
	s.reconcile()

	for _, name := range sup.DesiredServiceNames() {
		for _, ls := range sup.ServiceLogSources(name) {
			got = append(got, taggedSource{service: name, LogSource: ls})
		}
	}

	tl.mu.Lock()
	tracked := len(tl.cancels)
	tl.mu.Unlock()
	if tracked != len(got) {
		t.Fatalf("tailer tracks %d sources, want %d", tracked, len(got))
	}
	tl.stopAll()
}
