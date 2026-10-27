package main

import "testing"

// TestWillSelfClone covers the flag shapes runRunVerb has to recognise
// before deciding whether to skip the early task-file existence check.
// The k8s spawner builds argv via apps/core/ci/infrastructure/
// spawner_k8s.go:buildRunnerArgs, which uses the space-separated form
// (`-repo-url <url>`); the equals form is supported because Go's `flag`
// package accepts it and a hand-written invocation may use either.
func TestWillSelfClone(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"empty", nil, false},
		{"no repo-url flag", []string{"-repo", "/workspace", "-output", "json"}, false},
		{"space separated", []string{"-repo", "/workspace", "-repo-url", "https://github.com/x/y.git"}, true},
		{"double-dash space", []string{"--repo-url", "https://example.com/r.git"}, true},
		{"equals form", []string{"-repo-url=https://example.com/r.git"}, true},
		{"double-dash equals", []string{"--repo-url=https://example.com/r.git"}, true},
		{"empty value space", []string{"-repo-url", ""}, false},
		{"empty value equals", []string{"-repo-url="}, false},
		{"missing trailing value", []string{"-repo-url"}, false},
		{"flag deeper in argv", []string{"-output", "json", "-head-ref", "abc", "-repo-url", "https://x"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := willSelfClone(tc.args)
			if got != tc.want {
				t.Fatalf("willSelfClone(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestParseListenArgsDetectsInstall(t *testing.T) {
	o, install, ok := parseListenArgs([]string{"--listen", ":6464", "--install", "--advertise", "10.0.0.4"})
	addr, advertise := o.Addr, o.Advertise
	if !ok || !install {
		t.Fatalf("ok=%v install=%v", ok, install)
	}
	if addr != ":6464" || advertise != "10.0.0.4" {
		t.Fatalf("addr=%q advertise=%q — --install must not swallow the next flag's value", addr, advertise)
	}
}

func TestParseListenArgsWithoutListenIsInert(t *testing.T) {
	if _, _, ok := parseListenArgs([]string{"ci-skalpai", "-repo", "."}); ok {
		t.Fatal("a normal `sklp run` must not be taken over by the serve path")
	}
}

func TestParseListenArgsDefaultsAddr(t *testing.T) {
	o, _, ok := parseListenArgs([]string{"--listen"})
	addr := o.Addr
	if !ok || addr != ":6464" {
		t.Fatalf("ok=%v addr=%q", ok, addr)
	}
}

func TestParseListenArgsConcurrency(t *testing.T) {
	o, _, ok := parseListenArgs([]string{"--listen", "--concurrency", "4"})
	concurrency := o.Concurrency
	if !ok || concurrency != 4 {
		t.Fatalf("ok=%v concurrency=%d, want 4", ok, concurrency)
	}
}

func TestParseListenArgsConcurrencyDefaultsToOne(t *testing.T) {
	o, _, ok := parseListenArgs([]string{"--listen"})
	concurrency := o.Concurrency
	if !ok || concurrency != 1 {
		t.Fatalf("ok=%v concurrency=%d, want 1", ok, concurrency)
	}
}

func TestParseListenArgsConcurrencyIgnoresBadValue(t *testing.T) {
	o, _, ok := parseListenArgs([]string{"--listen", "--concurrency", "0"})
	concurrency := o.Concurrency
	if !ok || concurrency != 1 {
		t.Fatalf("ok=%v concurrency=%d, want 1 (non-positive value must not disable the gate)", ok, concurrency)
	}
}

func TestParseListenArgsAllowHostSteps(t *testing.T) {
	o, _, ok := parseListenArgs([]string{"--listen", "--allow-host-steps", "--advertise", "10.0.0.4"})
	if !ok || !o.AllowHostSteps || o.Advertise != "10.0.0.4" {
		t.Fatalf("ok=%v allow=%v advertise=%q", ok, o.AllowHostSteps, o.Advertise)
	}
	if o, _, _ := parseListenArgs([]string{"--listen"}); o.AllowHostSteps {
		t.Fatal("host steps must be off by default")
	}
}
