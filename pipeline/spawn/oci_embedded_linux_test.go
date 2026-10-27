package spawn

import (
	"bytes"
	"strings"
	"testing"
)

// TestMapEmbeddedServices_Maps verifies the neutral->runtime translation
// including env-map ordering and ready mapping.
func TestMapEmbeddedServices_Maps(t *testing.T) {
	in := []Service{
		{
			Name:      "db",
			Image:     "postgres:16",
			Command:   []string{"postgres", "-c", "x"},
			Env:       map[string]string{"B": "2", "A": "1"},
			DependsOn: []string{"migrate"},
			Ready:     &ServiceReady{TCP: "localhost:5432", Timeout: "10s"},
			Volumes:   []string{"pg:/data"},
		},
		{Name: "migrate", Image: "migrate:1", Oneshot: true},
	}
	var live bytes.Buffer
	out, err := mapEmbeddedServices(in, &live)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2, got %d", len(out))
	}
	db := out[0]
	if db.Name != "db" || db.Image != "postgres:16" {
		t.Errorf("db basics wrong: %+v", db)
	}
	// Env rendered as sorted KEY=value.
	if len(db.Env) != 2 || db.Env[0] != "A=1" || db.Env[1] != "B=2" {
		t.Errorf("env not sorted KEY=value: %v", db.Env)
	}
	if db.Ready == nil || db.Ready.TCP != "localhost:5432" || db.Ready.Timeout != "10s" {
		t.Errorf("ready not mapped: %+v", db.Ready)
	}
	if !out[1].Oneshot {
		t.Error("migrate oneshot lost")
	}
	if live.Len() != 0 {
		t.Errorf("no network warning expected, got: %q", live.String())
	}
}

// TestMapEmbeddedServices_CarriesRun keeps a shell service and forwards Run
// (the Space path runs it on the runner rootfs; no rejection).
func TestMapEmbeddedServices_CarriesRun(t *testing.T) {
	out, err := mapEmbeddedServices([]Service{{Name: "sh", Run: "sleep 1"}}, nil)
	if err != nil {
		t.Fatalf("run: service should be accepted: %v", err)
	}
	if len(out) != 1 || out[0].Run != "sleep 1" || out[0].Image != "" {
		t.Errorf("run not carried through: %+v", out)
	}
}

// TestMapEmbeddedServices_RejectsNoImage rejects image-less, run-less.
func TestMapEmbeddedServices_RejectsNoImage(t *testing.T) {
	_, err := mapEmbeddedServices([]Service{{Name: "empty"}}, nil)
	if err == nil {
		t.Fatal("expected error for image-less service")
	}
	if !strings.Contains(err.Error(), "neither image: nor run:") {
		t.Errorf("unexpected message: %v", err)
	}
}

// TestMapEmbeddedServices_NoNetworkWarning verifies the Space path emits NO
// loopback warning for bridge/ports services — the Space backend honours
// bridge networking, DNS-by-name and native ports, so the old warning is gone.
func TestMapEmbeddedServices_NoNetworkWarning(t *testing.T) {
	in := []Service{
		{Name: "db", Image: "postgres:16", Ports: []string{"5432:5432"}},
		{Name: "cache", Image: "redis:7", Network: "bridge"},
	}
	var live bytes.Buffer
	if _, err := mapEmbeddedServices(in, &live); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if live.Len() != 0 {
		t.Errorf("no network warning expected on the Space path, got: %q", live.String())
	}
}

// TestEmbeddedRunnerModeEnv locks the SKLP_RUNNER_MODE signal the embedded
// backend injects. It MUST stay equal to runner.RunnerModeEmbedded
// ("embedded-oci"); spawn cannot import runner (cycle), so a drift here
// must be mirrored there. The runner-side test reads the same value.
func TestEmbeddedRunnerModeEnv(t *testing.T) {
	if embeddedRunnerModeEnv != "SKLP_RUNNER_MODE=embedded-oci" {
		t.Fatalf("runner-mode env drifted: %q", embeddedRunnerModeEnv)
	}
}

func TestDefaultCICPUs(t *testing.T) {
	cases := map[int]int{1: 1, 2: 2, 4: 2, 8: 2, 10: 2, 12: 3, 32: 8}
	for host, want := range cases {
		if got := defaultCICPUs(host); got != want {
			t.Errorf("defaultCICPUs(%d) = %d, want %d", host, got, want)
		}
	}
}

func TestCICPUBudget_EnvOverride(t *testing.T) {
	t.Setenv(ciCPUsEnv, "5")
	if got := ciCPUBudget(); got != 5 {
		t.Fatalf("ciCPUBudget = %d, want 5", got)
	}
}
