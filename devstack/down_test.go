package devstack

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestStateRoundTrip writes a runState and reads it back, asserting the
// statefile is per-repo (devScopeID-keyed) and survives marshal.
func TestStateRoundTrip(t *testing.T) {
	repo := t.TempDir()
	want := runState{
		SupervisorPID: 4242,
		PGIDs:         []int{111, 222},
		OCIContainers: []string{"postgres", "nats"},
		OCIRuntime:    "docker",
		Network:       "sklp-dev-abc123",
	}
	if err := writeState(repo, "", want); err != nil {
		t.Fatalf("writeState: %v", err)
	}
	got, err := readState(repo, "")
	if err != nil {
		t.Fatalf("readState: %v", err)
	}
	if got == nil {
		t.Fatal("readState returned nil for an existing statefile")
	}
	if got.SupervisorPID != want.SupervisorPID || got.OCIRuntime != want.OCIRuntime ||
		got.Network != want.Network || len(got.PGIDs) != 2 || len(got.OCIContainers) != 2 {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, want)
	}

	removeState(repo, "")
	got, err = readState(repo, "")
	if err != nil {
		t.Fatalf("readState after remove: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil after removeState, got %+v", got)
	}
}

// TestReadState_NoFile distinguishes "nothing running" (nil, nil) from
// a read error.
func TestReadState_NoFile(t *testing.T) {
	st, err := readState(t.TempDir(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if st != nil {
		t.Fatalf("expected nil state, got %+v", st)
	}
}

// TestStopState_KillsProcessGroup launches a real `sleep` in its own
// process group (mirroring startProc's Setpgid) and asserts stopState
// reaps the whole group within the grace window.
func TestStopState_KillsProcessGroup(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pgid := cmd.Process.Pid
	// Reap in the background so the kernel doesn't keep a zombie that
	// would make signal-0 existence checks lie.
	go func() { _ = cmd.Wait() }()

	if pgidDead(pgid) {
		t.Fatal("process group reported dead immediately after start")
	}

	stopState(&runState{PGIDs: []int{pgid}}, 2*time.Second)

	// After stopState the group must be gone.
	deadline := time.After(time.Second)
	for {
		if pgidDead(pgid) {
			return
		}
		select {
		case <-deadline:
			t.Fatal("process group still alive after stopState")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
