package spacemanager

import (
	"testing"
	"time"
)

type lostExitWatcher struct{ *fakeDeathWatcher }

func (lostExitWatcher) ExitCode(int) (int, bool) { return 0, false }

// A job whose exit status no reaper ever stored (it died before its cgroup was
// joined, observed 2026-10-17) must not hold its pid forever: every later
// scheduled run was skipped as "still in flight".
func TestJob_LostExitCodeStillSettles(t *testing.T) {
	prev := deathSettleTimeout
	deathSettleTimeout = 50 * time.Millisecond
	defer func() { deathSettleTimeout = prev }()

	w := newFakeDeathWatcher()
	l := newFakeLauncher()
	desired := []Service{{Name: "job", Oneshot: true, Command: []string{"noop"}}}
	opts := testOpts()
	opts.Watcher = lostExitWatcher{w}
	a := NewAgent(l, nil, desired, opts)
	cancel, wg := startAgent(t, a)
	defer wg.Wait()
	defer cancel()

	waitWatch(t, w, "job-0", 2*time.Second)
	w.SimulateDeath("job-0")

	waitFor(t, 3*time.Second, "job relaunched after a death with no exit code", func() bool {
		return l.StartCount("job-0") >= 2
	})
}
