package plan

import (
	"runtime"
	"testing"
)

func TestParseCPUMax(t *testing.T) {
	cases := map[string]int{
		"max 100000\n":    0,
		"200000 100000\n": 2,
		"50000 100000":    1,
		"250000 100000":   3,
		"garbage":         0,
		"":                0,
		"0 100000":        0,
	}
	for in, want := range cases {
		if got := parseCPUMax(in); got != want {
			t.Errorf("parseCPUMax(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestCPUBudget_EnvWins(t *testing.T) {
	t.Setenv(cpuBudgetEnv, "3")
	if got := cpuBudget(); got != 3 {
		t.Fatalf("cpuBudget = %d, want 3", got)
	}
}

func TestCPUBudget_InvalidEnvFallsBack(t *testing.T) {
	t.Setenv(cpuBudgetEnv, "zero")
	got := cpuBudget()
	if got < 1 || got > runtime.NumCPU() {
		t.Fatalf("cpuBudget = %d, want within [1, %d]", got, runtime.NumCPU())
	}
}

func TestWithGOMAXPROCS(t *testing.T) {
	got := withGOMAXPROCS(map[string]string{"A": "1"}, 4)
	if got["GOMAXPROCS"] != "4" || got["A"] != "1" {
		t.Fatalf("got %v", got)
	}
	kept := withGOMAXPROCS(map[string]string{"GOMAXPROCS": "9"}, 4)
	if kept["GOMAXPROCS"] != "9" {
		t.Fatalf("step value overridden: %v", kept)
	}
	if out := withGOMAXPROCS(map[string]string{}, 0); len(out) != 0 {
		t.Fatalf("zero budget must not set GOMAXPROCS: %v", out)
	}
}
