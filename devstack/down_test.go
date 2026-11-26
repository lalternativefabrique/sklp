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

func TestRunningStates_FindsEveryStackOfTheRepo(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	repo := t.TempDir()
	other := t.TempDir()
	for _, w := range []struct{ repo, stack string }{{repo, "web"}, {repo, "messag"}, {other, "messag"}} {
		st := runState{SupervisorPID: 1, Embedded: true, EmbeddedSpace: devSpaceName(w.repo, w.stack), Repo: w.repo, Stack: w.stack}
		if err := writeState(w.repo, w.stack, st); err != nil {
			t.Fatalf("writeState: %v", err)
		}
	}

	got, err := runningStates(repo)
	if err != nil {
		t.Fatalf("runningStates: %v", err)
	}
	if len(got) != 2 || got[0].Stack != "messag" || got[1].Stack != "web" {
		t.Fatalf("want messag+web of this repo only, got %+v", got)
	}
}

func TestPickDownTarget(t *testing.T) {
	messag := runState{Stack: "messag"}
	web := runState{Stack: "web"}
	cases := []struct {
		name        string
		running     []runState
		positionals []string
		want        string
		wantCode    int
	}{
		{"named and running", []runState{messag, web}, []string{"messag"}, "messag", 0},
		{"named but not running", []runState{web}, []string{"messag"}, "", 0},
		{"unnamed single", []runState{messag}, nil, "messag", 0},
		{"unnamed none", nil, nil, "", 0},
		{"unnamed ambiguous", []runState{messag, web}, nil, "", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, code := pickDownTarget(tc.running, tc.positionals)
			got := ""
			if st != nil {
				got = st.Stack
			}
			if got != tc.want || code != tc.wantCode {
				t.Fatalf("got (%q, %d), want (%q, %d)", got, code, tc.want, tc.wantCode)
			}
		})
	}
}
