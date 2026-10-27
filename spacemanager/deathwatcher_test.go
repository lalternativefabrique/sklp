package spacemanager

// Tests using fakeDeathWatcher + fakeLauncher: verify reconciliation state
// transitions (Starting→Ready→Failed→Restarting, job Done/Failed, rollout
// cutover, dependency gating) without real processes or pidfds.
//
// Pattern: test wires fakeDeathWatcher + fakeLauncher → starts agent → waits
// for Watch (= instance launched) → calls SimulateDeath → asserts state.

import (
	"context"
	"sync"
	"testing"
	"time"
)

// newFakeAgent builds an agent with injected fake watcher + launcher and runs
// it. Returns the agent, cancel func, and WaitGroup.
func newFakeAgent(t *testing.T, w *fakeDeathWatcher, l *fakeLauncher, desired []Service, opts Options) (*Agent, context.CancelFunc, *sync.WaitGroup) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	opts.Watcher = w
	a := NewAgent(l, nil, desired, opts)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = a.Run(ctx) }()
	return a, cancel, &wg
}

// waitWatch polls until fakeDeathWatcher.IsWatched(id) is true, meaning the
// agent called Watch — i.e. the instance was launched and registered.
func waitWatch(t *testing.T, w *fakeDeathWatcher, id string, d time.Duration) {
	t.Helper()
	waitFor(t, d, "Watch("+id+")", func() bool { return w.IsWatched(id) })
}

// TestFakeDeathTransitionsToFailed: a death event moves the instance from
// Starting/Ready to Failed then Restarting, and the agent re-launches it.
func TestFakeDeathTransitionsToFailed(t *testing.T) {
	w := newFakeDeathWatcher()
	l := newFakeLauncher()
	desired := []Service{{Name: "svc", Command: []string{"noop"}}}
	a, cancel, wg := newFakeAgent(t, w, l, desired, testOpts())
	defer wg.Wait()
	defer cancel()

	// Wait for the first launch: agent calls Watch.
	waitWatch(t, w, "svc-0", 2*time.Second)
	if l.StartCount("svc-0") < 1 {
		t.Fatal("expected at least one Start before simulating death")
	}

	// Simulate death — agent's onDeath runs, transitions to Failed→Restarting.
	w.SimulateDeath("svc-0")

	// The agent should relaunch: StartCount goes to 2.
	waitFor(t, 2*time.Second, "service relaunched after fake death", func() bool {
		return l.StartCount("svc-0") >= 2
	})

	// State must not be stuck at Failed.
	state := a.State("svc")
	if state == Failed {
		t.Fatalf("state = Failed after relaunch, want Starting or Pending")
	}
}

// TestFakeMultipleDeathsBackoff: repeated deaths trigger restarts; after N
// SimulateDeath calls the StartCount reflects them.
func TestFakeMultipleDeathsBackoff(t *testing.T) {
	w := newFakeDeathWatcher()
	l := newFakeLauncher()
	opts := testOpts() // base 50ms, max 200ms
	desired := []Service{{Name: "crash", Command: []string{"noop"}}}
	_, cancel, wg := newFakeAgent(t, w, l, desired, opts)
	defer wg.Wait()
	defer cancel()

	// Drive 3 death-relaunch cycles: wait for Watch (= relaunched), then kill.
	for i := 0; i < 3; i++ {
		// Wait until the instance is watched (either initial or after backoff).
		waitFor(t, 2*time.Second, "crash-0 watched", func() bool {
			return w.IsWatched("crash-0")
		})
		w.SimulateDeath("crash-0")
		// Wait for it to be unwatched before the next iteration.
		waitFor(t, 2*time.Second, "crash-0 unwatched after death", func() bool {
			return !w.IsWatched("crash-0")
		})
	}

	// StartCount ≥ 3: one initial + at least one per death cycle.
	if l.StartCount("crash-0") < 3 {
		t.Fatalf("expected ≥3 starts after 3 deaths, got %d", l.StartCount("crash-0"))
	}
}

// TestFakeDeathTeardownCallsStop: on agent teardown, Stop is called for every
// managed instance.
func TestFakeDeathTeardownCallsStop(t *testing.T) {
	w := newFakeDeathWatcher()
	l := newFakeLauncher()
	desired := []Service{
		{Name: "a", Command: []string{"noop"}},
		{Name: "b", Command: []string{"noop"}},
	}
	_, cancel, wg := newFakeAgent(t, w, l, desired, testOpts())

	waitFor(t, 2*time.Second, "both instances started", func() bool {
		return l.StartCount("a-0") >= 1 && l.StartCount("b-0") >= 1
	})

	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	if l.StopCount("a-0") < 1 || l.StopCount("b-0") < 1 {
		t.Fatalf("expected Stop for both instances on teardown, a=%d b=%d",
			l.StopCount("a-0"), l.StopCount("b-0"))
	}
}

// TestFakeJobDoneOnCleanExit: a oneshot job whose death arrives with exit code
// 0 transitions to Done (terminal) and never relaunches.
func TestFakeJobDoneOnCleanExit(t *testing.T) {
	w := newFakeDeathWatcher()
	l := newFakeLauncher()
	desired := []Service{{Name: "migrate", Command: []string{"noop"}, Oneshot: true}}
	a, cancel, wg := newFakeAgent(t, w, l, desired, testOpts())
	defer wg.Wait()
	defer cancel()

	waitWatch(t, w, "migrate-0", 2*time.Second)
	pid := w.PID("migrate-0")

	// exit code 0 → Done
	w.SetExitCode(pid, 0)
	w.SimulateDeath("migrate-0")

	waitFor(t, 2*time.Second, "job reaches Done", func() bool {
		return a.State("migrate") == Done
	})

	startsBefore := l.StartCount("migrate-0")
	time.Sleep(100 * time.Millisecond)
	if l.StartCount("migrate-0") != startsBefore {
		t.Fatal("Done job was relaunched — must be terminal")
	}
}

// TestFakeJobFailedNonZero: a oneshot job with non-zero exit retries with
// backoff and eventually is held Failed after jobMaxRetries.
func TestFakeJobFailedNonZero(t *testing.T) {
	w := newFakeDeathWatcher()
	l := newFakeLauncher()
	opts := testOpts()
	desired := []Service{{Name: "job", Command: []string{"noop"}, Oneshot: true}}
	a, cancel, wg := newFakeAgent(t, w, l, desired, opts)
	defer wg.Wait()
	defer cancel()

	// Drive jobMaxRetries non-zero exits (the agent retries up to jobMaxRetries
	// times before holding Failed). Each cycle: wait for Watch (= relaunched
	// with new pid), set exit code on new pid, simulate death.
	for i := 0; i < jobMaxRetries; i++ {
		waitFor(t, 2*time.Second, "job-0 watched", func() bool {
			return w.IsWatched("job-0")
		})
		pid := w.PID("job-0")
		w.SetExitCode(pid, 1)
		w.SimulateDeath("job-0")
		waitFor(t, 2*time.Second, "job-0 unwatched after death", func() bool {
			return !w.IsWatched("job-0")
		})
	}

	waitFor(t, 3*time.Second, "job held Failed", func() bool {
		return a.State("job") == Failed
	})

	startsBefore := l.StartCount("job-0")
	time.Sleep(150 * time.Millisecond)
	if l.StartCount("job-0") != startsBefore {
		t.Fatal("held-Failed job was relaunched — must be fail-closed")
	}
}

// TestFakeDependencyGating: a service gated on a job stays Pending until the
// job reaches Done, then the agent launches it.
func TestFakeDependencyGating(t *testing.T) {
	w := newFakeDeathWatcher()
	l := newFakeLauncher()
	desired := []Service{
		{Name: "migrate", Command: []string{"noop"}, Oneshot: true},
		{Name: "api", Command: []string{"noop"}, DependsOn: []string{"migrate"}},
	}
	a, cancel, wg := newFakeAgent(t, w, l, desired, testOpts())
	defer wg.Wait()
	defer cancel()

	// migrate launches; api must NOT launch yet.
	waitWatch(t, w, "migrate-0", 2*time.Second)
	time.Sleep(50 * time.Millisecond)
	if l.StartCount("api-0") > 0 {
		t.Fatal("api started before migrate Done — dependency gate broken")
	}

	// Complete migrate.
	pid := w.PID("migrate-0")
	w.SetExitCode(pid, 0)
	w.SimulateDeath("migrate-0")

	waitFor(t, 2*time.Second, "migrate Done", func() bool {
		return a.State("migrate") == Done
	})

	// Now api must launch.
	waitFor(t, 2*time.Second, "api started after migrate Done", func() bool {
		return l.StartCount("api-0") >= 1
	})
}

// TestFakeRemoveServiceStopsAndUnwatches: RemoveService stops instances and
// they are unwatched (no lingering watch entry).
func TestFakeRemoveServiceStopsAndUnwatches(t *testing.T) {
	w := newFakeDeathWatcher()
	l := newFakeLauncher()
	desired := []Service{
		{Name: "keep", Command: []string{"noop"}},
		{Name: "drop", Command: []string{"noop"}},
	}
	a, cancel, wg := newFakeAgent(t, w, l, desired, testOpts())
	defer wg.Wait()
	defer cancel()

	waitWatch(t, w, "drop-0", 2*time.Second)

	a.RemoveService("drop")

	waitFor(t, 2*time.Second, "drop-0 stopped", func() bool {
		return l.StopCount("drop-0") >= 1
	})
	if a.HasService("drop") {
		t.Fatal("HasService(drop) true after RemoveService")
	}
	if w.IsWatched("drop-0") {
		t.Fatal("drop-0 still watched after RemoveService")
	}
	if !a.HasService("keep") {
		t.Fatal("RemoveService(drop) wrongly removed keep")
	}
}

// TestFakeOnAllReadyFires: OnAllReady is called once all services are watched
// (= launched). Since fakeLauncher + no readiness probe → Ready immediately
// after Watch, AllReady fires once both are up.
func TestFakeOnAllReadyFires(t *testing.T) {
	w := newFakeDeathWatcher()
	l := newFakeLauncher()
	fired := make(chan struct{}, 1)
	opts := testOpts()
	opts.OnAllReady = func() { fired <- struct{}{} }
	desired := []Service{
		{Name: "a", Command: []string{"noop"}},
		{Name: "b", Command: []string{"noop"}},
	}
	_, cancel, wg := newFakeAgent(t, w, l, desired, opts)
	defer wg.Wait()
	defer cancel()

	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("OnAllReady never fired")
	}
}

// A death that has to be retried must be able to read the same exit code again:
// onDeath asks twice (ExitCode, then ReapNow) and re-defers when neither
// answers. A read that consumed the code left a cleanly-exited oneshot stuck
// outside Done until reconcile relaunched it — the residual double-launch.
func TestReapRegistry_ReadIsRepeatable(t *testing.T) {
	const pid = 424242
	dropReaped(pid)
	storeReaped(pid, 0)
	defer dropReaped(pid)

	for i := range 3 {
		code, ok := takeReaped(pid)
		if !ok {
			t.Fatalf("read %d: exit code gone; a retried death can never settle", i+1)
		}
		if code != 0 {
			t.Fatalf("read %d: code = %d, want 0", i+1, code)
		}
	}
}

// Settling a death releases the entry, so the registry does not grow with codes
// nobody will ask for again.
func TestReapRegistry_DropForgetsCode(t *testing.T) {
	const pid = 424243
	storeReaped(pid, 3)
	dropReaped(pid)

	if _, ok := takeReaped(pid); ok {
		t.Fatal("a settled death must not keep its exit code")
	}
}
