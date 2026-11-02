package devstack

import (
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// installSubreaper marks this process as a "child subreaper" so that any
// orphan a service leaves behind is re-parented to US instead of to init
// (PID 1). Without it, a shell service like
//
//	trap "..." TERM; touch m; while true; do sleep 0.05; done
//
// loses the race in proc.stop(): pgrep snapshots the children once, but the
// `while` loop forks a fresh `sleep` ~20×/s, and the one in flight when the
// shell exits is re-parented to init and escapes the kill sweep. As a
// subreaper we inherit that orphan and can wait4() it away.
//
// Idempotent and cheap (a single prctl); guarded by a Once so concurrent
// teardowns don't each call it. Linux ≥ 3.4. Best-effort: a failure only
// means we fall back to the pgrep sweep, which is the pre-existing behaviour.
var subreaperOnce sync.Once

func installSubreaper() {
	subreaperOnce.Do(func() {
		_ = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
	})
}

// reapOrphans drains re-parented orphans until none remain or the deadline
// passes. Each pass wait4()s every immediately-available zombie (WNOHANG);
// between passes we briefly yield so a service mid-shutdown (whose `while`
// loop forks one last `sleep` after the kill) has a moment to have that
// child re-parented to us and exit, at which point the next pass reaps it.
//
// This is the closing half of the subreaper: installSubreaper() guarantees
// the orphan lands on us; reapOrphans() collects it. Bounded by `budget`
// so a pathological forker can never hang teardown.
func reapOrphans(budget time.Duration) {
	deadline := time.Now().Add(budget)
	for {
		reaped := false
		for {
			var ws syscall.WaitStatus
			// wait4(-1, WNOHANG): reap ANY child that is ready now,
			// including orphans re-parented to us by the subreaper.
			pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
			if pid <= 0 || err != nil {
				break // ECHILD (no children) or none ready right now
			}
			reaped = true
		}
		if time.Now().After(deadline) {
			return
		}
		if !reaped {
			// Nothing was waiting; give a late re-fork time to arrive.
			time.Sleep(10 * time.Millisecond)
			// One more probe pass after the yield; if it too comes up
			// empty we're done.
			var ws syscall.WaitStatus
			if pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil); pid <= 0 || err != nil {
				return
			}
		}
	}
}

// reapExitStatus collects the exit status of a container init this process
// has been handed as subreaper: runc runs a service detached and exits, so
// its init is re-parented here, and its zombie waits for this wait4. Bounded,
// since a status that never comes must not stall the stack.
func reapExitStatus(pid int) (code int, known bool) {
	deadline := time.Now().Add(3 * time.Second)
	for {
		var ws unix.WaitStatus
		wpid, err := unix.Wait4(pid, &ws, unix.WNOHANG, nil)
		switch {
		case wpid == pid && err == nil:
			if ws.Signaled() {
				return 128 + int(ws.Signal()), true
			}
			return ws.ExitStatus(), true
		case err == unix.ECHILD:
			return 0, false
		}
		if time.Now().After(deadline) {
			return 0, false
		}
		time.Sleep(50 * time.Millisecond)
	}
}
