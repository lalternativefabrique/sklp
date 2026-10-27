package spacemanager

// Tests for the real pidfdWatcher (not the fake): they exercise Close() against
// live goroutines, pidfds and a saturated deathCh — the path that deadlocked
// ci-sklp intermittently.

import (
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sync"
	"testing"
	"time"
)

func newTestPidfdWatcher(t *testing.T, ctx context.Context) *pidfdWatcher {
	t.Helper()
	w := newPidfdWatcher(log.New(io.Discard, "", 0))
	if err := w.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return w
}

// waitChanFull blocks until deathCh is saturated, i.e. pollLoop has parked on a
// send with nobody reading — the precondition the hang needs. Polling the real
// state beats sleeping a guessed duration.
func waitChanFull(t *testing.T, w *pidfdWatcher, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if len(w.deathCh) == cap(w.deathCh) {
			return
		}
		w.Wake()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("deathCh never filled (%d/%d) — cannot exercise the blocking send", len(w.deathCh), cap(w.deathCh))
}

// closeWithin fails the test if Close() does not return before d. It is the
// assertion that matters here: a hung Close is exactly the production symptom
// (the agent never shuts down) and the CI symptom (10m test timeout).
func closeWithin(t *testing.T, w *pidfdWatcher, d time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); w.Close() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("Close() did not return within %s — pollLoop is stuck", d)
	}
}

// spawnDead registers a short-lived process with the watcher, then lets it die
// so its pidfd becomes readable.
//
// Watch MUST happen while the process is still alive: a watcher from an earlier
// test in this package installed PR_SET_CHILD_SUBREAPER, and its drainReap
// calls Wait4(-1) — it harvests every child of the test binary, not just its
// own. Reaping first would race that away and make PidfdOpen fail with ESRCH.
func spawnDead(t *testing.T, w *pidfdWatcher, id string) {
	t.Helper()
	cmd := exec.Command("sleep", "0.05")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := w.Watch(id, cmd.Process.Pid); err != nil {
		t.Fatalf("Watch(%s): %v", id, err)
	}
	_ = cmd.Wait() // may return an error if a foreign reaper won; irrelevant here
}

// TestPidfdWatcher_CloseWithNoReader is the regression test for the hang.
//
// deathCh is buffered, so a handful of deaths never surfaces the bug. Here more
// deaths than the buffer holds land while nobody reads Deaths() — the situation
// a scale-down creates. pollLoop must abandon its send once done is closed
// instead of blocking forever and stranding Close() on wg.Wait().
func TestPidfdWatcher_CloseWithNoReader(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := newTestPidfdWatcher(t, ctx)

	// Overshoot the buffer: cap+8 corpses, none of them ever consumed.
	for i := 0; i < cap(w.deathCh)+8; i++ {
		spawnDead(t, w, fmt.Sprintf("svc-%d", i))
	}
	waitChanFull(t, w, 10*time.Second)

	closeWithin(t, w, 5*time.Second)
}

// Close must stay idempotent and prompt when called twice (sync.Once guards the
// body; the second call must not wait on anything).
func TestPidfdWatcher_CloseTwice(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := newTestPidfdWatcher(t, ctx)

	closeWithin(t, w, 5*time.Second)
	closeWithin(t, w, time.Second)
}

// A cancelled context must also release pollLoop's pending send, so Close()
// returns even when shutdown came from ctx rather than from Close itself.
func TestPidfdWatcher_CloseAfterContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	w := newTestPidfdWatcher(t, ctx)

	for i := 0; i < cap(w.deathCh)+8; i++ {
		spawnDead(t, w, fmt.Sprintf("svc-%d", i))
	}
	waitChanFull(t, w, 10*time.Second)

	cancel()
	closeWithin(t, w, 5*time.Second)
}

// Deaths must still be delivered on the happy path: the guarded send must not
// drop events while a reader is present.
func TestPidfdWatcher_DeliversDeathToReader(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := newTestPidfdWatcher(t, ctx)
	defer w.Close()

	spawnDead(t, w, "svc-1")
	w.Wake()

	select {
	case id := <-w.Deaths():
		if id != "svc-1" {
			t.Fatalf("death id = %q, want svc-1", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no death delivered")
	}
}

// requeueDeath calls Wake() from its own goroutine, so Wake races Close()
// closing wakeFD and resetting it to -1. Under -race this fails on the
// unsynchronised field; without the detector it can write into a descriptor
// number already reassigned to an unrelated file.
func TestPidfdWatcher_WakeDuringCloseIsRaceFree(t *testing.T) {
	for i := 0; i < 50; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		w := newTestPidfdWatcher(t, ctx)

		var wg sync.WaitGroup
		start := make(chan struct{})
		for j := 0; j < 4; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				w.Wake()
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			w.Close()
		}()
		close(start)
		wg.Wait()
		cancel()
	}
}
