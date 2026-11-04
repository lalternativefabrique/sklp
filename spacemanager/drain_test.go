package spacemanager

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func startDrainAgent(t *testing.T, soak, drain time.Duration, open *atomic.Int32) (*Agent, *stopRecorder, *fakeProxy) {
	t.Helper()
	rc := newFakeChecker(30 * time.Millisecond)
	rc.set("api-0", "pass")
	rc.set("api-v1-0", "pass")
	probe := &Probe{TCP: "127.0.0.1:1"}
	stops := &stopRecorder{Launcher: newExecLauncher(), seen: map[string]bool{}}
	px := newFakeProxy()
	opts := proxyOpts(nil, px, time.Minute)
	opts.Soak = soak
	opts.Drain = drain
	a := NewAgent(stops, rc, []Service{rolloutSpec("v1", "sleep 30", probe)}, opts)
	a.connCounter = func(inst *instance) (int, bool) {
		if inst.id != "api-0" {
			return 0, true
		}
		return int(open.Load()), true
	}
	cancel, wg := startAgent(t, a)
	t.Cleanup(func() { cancel(); wg.Wait() })
	waitFor(t, 3*time.Second, "v1 ready and routed", func() bool {
		return a.AllReady() && px.has("api", "exec://api-0")
	})
	a.Reconfigure(rolloutSpec("v2", "sleep 30", probe))
	waitFor(t, 3*time.Second, "cutover to v2", func() bool {
		return px.has("api", "exec://api-v1-0") && !px.has("api", "exec://api-0")
	})
	return a, stops, px
}

func rolloutPhaseOf(a *Agent, name string) string {
	rs := a.Rollouts(name)
	if len(rs) == 0 {
		return ""
	}
	return rs[len(rs)-1].Result
}

func TestRollout_DrainKeepsPreviousWhileConnectionsAreEstablished(t *testing.T) {
	var open atomic.Int32
	open.Store(1)
	a, stops, _ := startDrainAgent(t, 100*time.Millisecond, 10*time.Second, &open)

	waitFor(t, 3*time.Second, "soak over, draining", func() bool {
		return rolloutPhaseOf(a, "api") == RolloutDraining
	})
	time.Sleep(400 * time.Millisecond)
	if stops.wasStopped("api-0") {
		t.Fatal("previous version stopped while a connection was still established to it")
	}

	open.Store(0)
	waitFor(t, 4*time.Second, "previous version stopped once drained", func() bool {
		return stops.wasStopped("api-0")
	})
	rec, ok := lastRollout(a, "api")
	if !ok || rec.Result != RolloutSucceeded || rec.Reason != "" {
		t.Fatalf("expected a clean succeeded record, got %+v", rec)
	}
}

func TestRollout_DrainCapStopsPreviousAndNotesWhatWasCut(t *testing.T) {
	var open atomic.Int32
	open.Store(2)
	a, stops, _ := startDrainAgent(t, 50*time.Millisecond, 300*time.Millisecond, &open)

	waitFor(t, 5*time.Second, "previous version stopped at the drain cap", func() bool {
		return stops.wasStopped("api-0")
	})
	rec, ok := lastRollout(a, "api")
	if !ok || rec.Result != RolloutSucceeded {
		t.Fatalf("expected a succeeded record, got %+v", rec)
	}
	if !strings.Contains(rec.Reason, "drain cap") || !strings.Contains(rec.Reason, "2 connection") {
		t.Fatalf("record must say the cap cut 2 connections, got %q", rec.Reason)
	}
}

func TestRollout_SpecDuringDrainWaitsForIt(t *testing.T) {
	var open atomic.Int32
	open.Store(1)
	a, stops, px := startDrainAgent(t, 50*time.Millisecond, 10*time.Second, &open)
	waitFor(t, 3*time.Second, "draining", func() bool {
		return rolloutPhaseOf(a, "api") == RolloutDraining
	})

	probe := &Probe{TCP: "127.0.0.1:1"}
	a.Reconfigure(rolloutSpec("v3", "sleep 30", probe))
	time.Sleep(300 * time.Millisecond)
	if stops.wasStopped("api-0") || !px.has("api", "exec://api-v1-0") {
		t.Fatal("a spec arriving during the drain must not disturb the generation serving nor the one draining")
	}

	open.Store(0)
	waitFor(t, 6*time.Second, "v3 rolled after the drain", func() bool {
		snap := a.Snapshot()
		return len(snap) > 0 && snap[0].Image == "v3" && stops.wasStopped("api-0")
	})
}

func TestRollout_WithoutAnObservableCounterSoakEndStopsPrevious(t *testing.T) {
	rc := newFakeChecker(30 * time.Millisecond)
	rc.set("api-0", "pass")
	rc.set("api-v1-0", "pass")
	probe := &Probe{TCP: "127.0.0.1:1"}
	a, stops, _ := startRolloutAgent(t, rc, 100*time.Millisecond, rolloutSpec("v1", "sleep 30", probe))
	if a.connCounter != nil {
		t.Fatal("the exec launcher exposes no addresses, so no counter is expected")
	}
	a.Reconfigure(rolloutSpec("v2", "sleep 30", probe))
	waitFor(t, 4*time.Second, "previous version stopped right after the soak", func() bool {
		return stops.wasStopped("api-0")
	})
}
