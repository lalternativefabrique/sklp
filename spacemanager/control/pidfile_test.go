package control

import (
	"os"
	"testing"
)

// TestSupervisorPidRoundTrip: write -> read -> remove of the pidfile, plus a
// missing-pidfile read surfacing os.ErrNotExist.
func TestSupervisorPidRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if _, err := ReadSupervisorPid("c"); !os.IsNotExist(err) {
		t.Fatalf("read of absent pidfile = %v, want not-exist", err)
	}

	if err := WriteSupervisorPid("c", 4242); err != nil {
		t.Fatalf("write pidfile: %v", err)
	}
	got, err := ReadSupervisorPid("c")
	if err != nil {
		t.Fatalf("read pidfile: %v", err)
	}
	if got != 4242 {
		t.Fatalf("pid = %d, want 4242", got)
	}

	RemoveSupervisorPid("c")
	if _, err := ReadSupervisorPid("c"); !os.IsNotExist(err) {
		t.Fatalf("read after remove = %v, want not-exist", err)
	}
}

// TestSignalSupervisorDownNoPidfile: down on a space with no pidfile (systemd
// or never deployed) is a clean no-op.
func TestSignalSupervisorDownNoPidfile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	signalled, err := SignalSupervisorDown("nope")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if signalled {
		t.Fatal("must not signal anything when no pidfile exists")
	}
}

// TestSignalSupervisorDownStalePid: a pidfile pointing at a dead process is
// detected via signal 0, ignored, and the stale pidfile is removed.
func TestSignalSupervisorDownStalePid(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	stale := impossiblePid(t)
	if err := WriteSupervisorPid("c", stale); err != nil {
		t.Fatalf("write pidfile: %v", err)
	}
	signalled, err := SignalSupervisorDown("c")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if signalled {
		t.Fatal("must not signal a dead process")
	}
	if _, err := ReadSupervisorPid("c"); !os.IsNotExist(err) {
		t.Fatal("stale pidfile should be removed")
	}
}

// TestProcessAlive: the running test process is alive; a dead/invalid pid is not.
func TestProcessAlive(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Fatal("current process must report alive")
	}
	if processAlive(0) || processAlive(-1) {
		t.Fatal("non-positive pid must report not alive")
	}
	if processAlive(impossiblePid(t)) {
		t.Fatal("an unused high pid must report not alive")
	}
}

// impossiblePid returns a pid that is (almost certainly) not in use: it scans
// up from a high value until syscall.Kill(pid,0) reports ESRCH.
func impossiblePid(t *testing.T) int {
	t.Helper()
	for pid := 1 << 22; pid < (1<<22)+10000; pid++ {
		if !processAlive(pid) {
			return pid
		}
	}
	t.Skip("could not find an unused pid")
	return 0
}
