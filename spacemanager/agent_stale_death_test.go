package spacemanager

import (
	"testing"
	"time"
)

// The previous run's pidfd can fire after the instance was relaunched under
// the same id. That death is not the new run's: acting on it killed a run that
// had just turned Ready and relaunched it for nothing.
func TestStaleDeathAfterRestartLeavesTheNewRunAlone(t *testing.T) {
	w := newFakeDeathWatcher()
	l := newFakeLauncher()
	desired := []Service{{Name: "svc", Command: []string{"noop"}}}
	a, cancel, wg := newFakeAgent(t, w, l, desired, testOpts())
	defer wg.Wait()
	defer cancel()

	waitWatch(t, w, "svc-0", 2*time.Second)
	oldPID := l.PID("svc-0")

	if !a.RestartService("svc") {
		t.Fatal("RestartService(svc) = false")
	}
	waitFor(t, 2*time.Second, "svc-0 relaunched", func() bool { return l.StartCount("svc-0") == 2 })
	waitFor(t, 2*time.Second, "new run watched", func() bool { return l.PID("svc-0") != oldPID && w.IsWatched("svc-0") })
	newPID := l.PID("svc-0")

	w.SimulateDeathOf("svc-0", oldPID)
	time.Sleep(300 * time.Millisecond)
	if got := l.StartCount("svc-0"); got != 2 {
		t.Fatalf("a death of the previous pid %d relaunched the new run %d: %d starts, want 2", oldPID, newPID, got)
	}

	w.SimulateDeathOf("svc-0", newPID)
	waitFor(t, 2*time.Second, "a death of the current pid still relaunches", func() bool { return l.StartCount("svc-0") == 3 })
}
