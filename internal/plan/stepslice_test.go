package plan

import (
	"reflect"
	"testing"
)

func TestHalfHost(t *testing.T) {
	for host, want := range map[int]int{1: 1, 2: 2, 3: 2, 4: 2, 20: 10, 64: 32} {
		if got := halfHost(host); got != want {
			t.Errorf("halfHost(%d) = %d, want %d", host, got, want)
		}
	}
}

func TestStepSliceCPUsHonoursBudgetEnv(t *testing.T) {
	t.Setenv(cpuBudgetEnv, "3")
	if got := stepSliceCPUs(); got != 3 {
		t.Fatalf("stepSliceCPUs() = %d, want 3", got)
	}
}

func TestReachesUserBus(t *testing.T) {
	if reachesUserBus([]string{"PATH=/bin", "HOME=/h"}) {
		t.Error("env without a bus address must not be wrapped")
	}
	if !reachesUserBus([]string{"PATH=/bin", "XDG_RUNTIME_DIR=/run/user/1000"}) {
		t.Error("XDG_RUNTIME_DIR reaches the user bus")
	}
}

func TestStepSliceArgv(t *testing.T) {
	got := stepSliceArgv("/usr/bin/systemd-run", []string{"/bin/sh", "-c", "go build ./..."})
	want := []string{"/usr/bin/systemd-run", "--user", "--scope", "--quiet", "--collect", "--slice=sklp-steps.slice", "--", "/bin/sh", "-c", "go build ./..."}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}
