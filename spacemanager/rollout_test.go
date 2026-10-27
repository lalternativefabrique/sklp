package spacemanager

import (
	"strings"
	"testing"
	"time"
)

func rolloutSpec(image, cmd string, probe *Probe) Service {
	return Service{
		Name:    "api",
		Image:   image,
		Command: []string{"sh", "-c", cmd},
		Port:    8080,
		Ready:   probe,
	}
}

func lastRollout(a *Agent, name string) (RolloutRecord, bool) {
	rs := a.Rollouts(name)
	for i := len(rs) - 1; i >= 0; i-- {
		switch rs[i].Result {
		case RolloutRolling, RolloutSoaking:
			continue
		}
		return rs[i], true
	}
	return RolloutRecord{}, false
}

func startRolloutAgent(t *testing.T, rc *fakeChecker, soak time.Duration, v1 Service) (*Agent, *stopRecorder, *fakeProxy) {
	t.Helper()
	stops := &stopRecorder{Launcher: newExecLauncher(), seen: map[string]bool{}}
	px := newFakeProxy()
	opts := proxyOpts(nil, px, time.Minute)
	opts.Soak = soak
	a := NewAgent(stops, rc, []Service{v1}, opts)
	cancel, wg := startAgent(t, a)
	t.Cleanup(func() { cancel(); wg.Wait() })
	waitFor(t, 3*time.Second, "v1 ready and routed", func() bool {
		return a.AllReady() && px.has("api", "exec://api-0")
	})
	return a, stops, px
}

func TestRollout_CrashDuringSoakRoutesBackToPrevious(t *testing.T) {
	rc := newFakeChecker(30 * time.Millisecond)
	rc.set("api-0", "pass")
	rc.set("api-v1-0", "pass")
	probe := &Probe{TCP: "127.0.0.1:1"}
	a, stops, px := startRolloutAgent(t, rc, 5*time.Second, rolloutSpec("v1", "sleep 30", probe))

	a.Reconfigure(rolloutSpec("v2", "sleep 0.4; exit 3", probe))

	waitFor(t, 3*time.Second, "cutover to v2", func() bool {
		return px.has("api", "exec://api-v1-0") && !px.has("api", "exec://api-0")
	})
	if stops.wasStopped("api-0") {
		t.Fatal("previous version stopped before the soak elapsed")
	}

	waitFor(t, 4*time.Second, "routed back to v1 after the crash", func() bool {
		return px.has("api", "exec://api-0") && !px.has("api", "exec://api-v1-0")
	})
	if stops.wasStopped("api-0") {
		t.Fatal("previous version was stopped although the new one crashed inside the soak")
	}
	waitFor(t, 2*time.Second, "service ready again on v1", a.AllReady)

	rec, ok := lastRollout(a, "api")
	if !ok || rec.Result != RolloutRolledBack {
		t.Fatalf("expected a rolled_back record, got %+v", rec)
	}
	// No ExitCode assertion: every agent in this package installs a subreaper
	// draining with Wait4(-1), so a neighbouring test reaps this child first and
	// the code reads back unknown.
	if !strings.Contains(rec.Reason, "soak") || rec.FromImage != "v1" || rec.ToImage != "v2" {
		t.Fatalf("unexpected record: %+v", rec)
	}
	snap := a.Snapshot()
	if snap[0].Image != "v1" {
		t.Fatalf("snapshot reports image %q, want the serving v1", snap[0].Image)
	}
	if a.ServiceDiffers(rolloutSpec("v2", "sleep 0.4; exit 3", probe)) {
		t.Fatal("the rolled-back spec must read as unchanged until a different one arrives")
	}
	if !a.ServiceDiffers(rolloutSpec("v3", "sleep 30", probe)) {
		t.Fatal("a different spec must still be applied")
	}
}

func TestRollout_CrashLoopBeforeReadyRollsBackWithoutWaitingForTimeout(t *testing.T) {
	rc := newFakeChecker(30 * time.Millisecond)
	rc.set("api-0", "pass")
	rc.set("api-v1-0", "block")
	probe := &Probe{TCP: "127.0.0.1:1"}
	a, stops, px := startRolloutAgent(t, rc, time.Minute, rolloutSpec("v1", "sleep 30", probe))

	a.Reconfigure(rolloutSpec("v2", "exit 1", probe))

	waitFor(t, 5*time.Second, "rollout rolled back after repeated crashes", func() bool {
		rec, ok := lastRollout(a, "api")
		return ok && rec.Result == RolloutRolledBack
	})
	rec, _ := lastRollout(a, "api")
	if !strings.Contains(rec.Reason, "before becoming Ready") {
		t.Fatalf("unexpected record: %+v", rec)
	}
	if stops.wasStopped("api-0") || !px.has("api", "exec://api-0") {
		t.Fatal("previous version must stay up and routed")
	}
	if !a.AllReady() {
		t.Fatal("service must still count Ready on the previous version")
	}

	a.Reconfigure(rolloutSpec("v2", "exit 1", probe))
	time.Sleep(200 * time.Millisecond)
	if n := len(a.Rollouts("api")); n != 1 {
		t.Fatalf("re-applying the rolled-back spec started another rollout (%d records)", n)
	}
}

func TestRollout_SoakElapsedPromotesAndStopsPrevious(t *testing.T) {
	rc := newFakeChecker(30 * time.Millisecond)
	rc.set("api-0", "pass")
	rc.set("api-v1-0", "pass")
	probe := &Probe{TCP: "127.0.0.1:1"}
	a, stops, px := startRolloutAgent(t, rc, 300*time.Millisecond, rolloutSpec("v1", "sleep 30", probe))

	a.Reconfigure(rolloutSpec("v2", "sleep 30", probe))

	waitFor(t, 4*time.Second, "previous version stopped after the soak", func() bool {
		return stops.wasStopped("api-0")
	})
	if !px.has("api", "exec://api-v1-0") || px.has("api", "exec://api-0") {
		t.Fatal("proxy must point at v2 only")
	}
	rec, ok := lastRollout(a, "api")
	if !ok || rec.Result != RolloutSucceeded {
		t.Fatalf("expected a succeeded record, got %+v", rec)
	}
	if snap := a.Snapshot(); snap[0].Image != "v2" {
		t.Fatalf("snapshot reports image %q, want v2", snap[0].Image)
	}
}

func TestRollout_PerServiceSoakOverridesDefault(t *testing.T) {
	rc := newFakeChecker(30 * time.Millisecond)
	rc.set("api-0", "pass")
	rc.set("api-v1-0", "pass")
	probe := &Probe{TCP: "127.0.0.1:1", Soak: "200ms"}
	a, stops, _ := startRolloutAgent(t, rc, time.Hour, rolloutSpec("v1", "sleep 30", probe))

	a.Reconfigure(rolloutSpec("v2", "sleep 30", probe))

	waitFor(t, 4*time.Second, "previous version stopped after the service's own soak", func() bool {
		return stops.wasStopped("api-0")
	})
	if rec, ok := lastRollout(a, "api"); !ok || rec.Result != RolloutSucceeded {
		t.Fatalf("expected a succeeded record, got %+v", rec)
	}
}

func TestRollout_ReconfigureMidRolloutSupersedesInFlightGeneration(t *testing.T) {
	rc := newFakeChecker(30 * time.Millisecond)
	rc.set("api-0", "pass")
	rc.set("api-v1-0", "block")
	rc.set("api-v2-0", "pass")
	probe := &Probe{TCP: "127.0.0.1:1"}
	a, stops, px := startRolloutAgent(t, rc, 200*time.Millisecond, rolloutSpec("v1", "sleep 30", probe))

	a.Reconfigure(rolloutSpec("v2", "sleep 30", probe))
	waitFor(t, 3*time.Second, "v2 started", func() bool {
		return len(stops.Launcher.(*execLauncher).pidsOf("api-v1-0")) == 1
	})

	a.Reconfigure(rolloutSpec("v3", "sleep 30", probe))

	waitFor(t, 3*time.Second, "superseded generation stopped", func() bool {
		return stops.wasStopped("api-v1-0")
	})
	if stops.wasStopped("api-0") || !px.has("api", "exec://api-0") {
		t.Fatal("previous version must stay up and routed while v3 comes up")
	}
	waitFor(t, 5*time.Second, "v3 promoted", func() bool {
		rec, ok := lastRollout(a, "api")
		return ok && rec.Result == RolloutSucceeded && rec.ToImage == "v3"
	})
	rs := a.Rollouts("api")
	if len(rs) != 2 || rs[0].Result != RolloutSuperseded || rs[0].ToImage != "v2" {
		t.Fatalf("expected [superseded v2, succeeded v3], got %+v", rs)
	}
	if !px.has("api", "exec://api-v2-0") || px.has("api", "exec://api-0") {
		t.Fatal("proxy must point at v3 only")
	}
}
