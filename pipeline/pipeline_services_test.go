package pipeline

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

func TestRequestedServices_FiltersToUsedNames(t *testing.T) {
	p := Pipeline{
		Services: map[string]Service{
			"db":     {Image: "postgres:16"},
			"cache":  {Image: "redis:7"},
			"unused": {Image: "busybox"},
		},
		Steps: []Step{
			{Name: "a", Services: []string{"db"}},
			{Name: "b", Services: []string{"db", "cache"}},
		},
	}
	got := requestedServices(p, nil)
	if len(got) != 2 {
		t.Fatalf("expected 2 requested services, got %d (%v)", len(got), got)
	}
	if _, ok := got["db"]; !ok {
		t.Error("missing db")
	}
	if _, ok := got["cache"]; !ok {
		t.Error("missing cache")
	}
	if _, ok := got["unused"]; ok {
		t.Error("unused must be filtered out")
	}
}

// A diff that gates every backend step out of scope must NOT request
// the services those steps reference — booting postgres + nats for a
// 100% frontend change is a slow, racy waste that surfaces as
// "service not ready" failures on runs that had nothing to do.
func TestRequestedServices_FiltersOutOfScopeSteps(t *testing.T) {
	p := Pipeline{
		Services: map[string]Service{
			"postgres": {Image: "postgres:16"},
			"nats":     {Image: "nats:latest"},
		},
		Steps: []Step{
			{Name: "go-test", Services: []string{"postgres", "nats"}},
			{Name: "node-test"},
		},
	}
	// Frontend-only diff: only node-test is in scope.
	included := map[string]bool{"node-test": true}
	if got := requestedServices(p, included); got != nil {
		t.Fatalf("expected no services requested when no in-scope step needs them, got %v", got)
	}
}

func TestRequestedServices_NoSteps(t *testing.T) {
	p := Pipeline{Services: map[string]Service{"db": {Image: "x"}}}
	if got := requestedServices(p, nil); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

// TestStartServicesForPipeline_SkipsUnderK8s confirms the K8s contract:
// SKLP_RUNNER_MODE=k8s bypasses the orchestrator hook entirely.
func TestStartServicesForPipeline_SkipsUnderK8s(t *testing.T) {
	t.Setenv(RunnerModeEnv, RunnerModeK8s)

	called := false
	StartPipelineServicesHook = func(
		ctx context.Context, requested map[string]Service,
		env map[string]string, workdir string,
		stdout, stderr io.Writer,
	) (func(), map[string]string, error) {
		called = true
		return func() {}, nil, nil
	}
	t.Cleanup(func() { StartPipelineServicesHook = nil })

	p := Pipeline{
		Services: map[string]Service{"db": {Image: "postgres:16"}},
		Steps:    []Step{{Name: "a", Services: []string{"db"}}},
	}
	var stderr bytes.Buffer
	teardown, _, err := startServicesForPipeline(context.Background(), p, nil, "/tmp", io.Discard, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	teardown()
	if called {
		t.Fatal("hook must not be called under SKLP_RUNNER_MODE=k8s")
	}
	if !strings.Contains(stderr.String(), "services managed externally") {
		t.Errorf("expected external-services skip log, got: %q", stderr.String())
	}
}

// TestStartServicesForPipeline_NoServicesNoOp verifies the cheap path:
// no requested services ⇒ no hook call, no log, no error.
func TestStartServicesForPipeline_NoServicesNoOp(t *testing.T) {
	called := false
	StartPipelineServicesHook = func(
		ctx context.Context, requested map[string]Service,
		env map[string]string, workdir string,
		stdout, stderr io.Writer,
	) (func(), map[string]string, error) {
		called = true
		return func() {}, nil, nil
	}
	t.Cleanup(func() { StartPipelineServicesHook = nil })

	p := Pipeline{Steps: []Step{{Name: "only", Run: "true"}}}
	var stderr bytes.Buffer
	teardown, _, err := startServicesForPipeline(context.Background(), p, nil, "/tmp", io.Discard, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	teardown() // must be safe even when no services were started
	if called {
		t.Fatal("hook must not be called when no service is requested")
	}
}

// TestStartServicesForPipeline_HookNilSkips ensures the executor stays
// usable when the hook hasn't been wired (the case in runner-only
// tests, by design).
func TestStartServicesForPipeline_HookNilSkips(t *testing.T) {
	StartPipelineServicesHook = nil

	p := Pipeline{
		Services: map[string]Service{"db": {Image: "postgres:16"}},
		Steps:    []Step{{Name: "a", Services: []string{"db"}}},
	}
	var stderr bytes.Buffer
	teardown, _, err := startServicesForPipeline(context.Background(), p, nil, "/tmp", io.Discard, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	teardown()
	if !strings.Contains(stderr.String(), "services orchestrator not registered") {
		t.Errorf("expected skip log, got: %q", stderr.String())
	}
}

// TestStartServicesForPipeline_DelegatesToHook covers the happy path
// outside K8s: hook gets called with the filtered service set and the
// pipeline env overlay.
func TestStartServicesForPipeline_DelegatesToHook(t *testing.T) {
	var (
		gotServices map[string]Service
		gotEnv      map[string]string
	)
	StartPipelineServicesHook = func(
		ctx context.Context, requested map[string]Service,
		env map[string]string, workdir string,
		stdout, stderr io.Writer,
	) (func(), map[string]string, error) {
		gotServices = requested
		gotEnv = env
		return func() {}, nil, nil
	}
	t.Cleanup(func() { StartPipelineServicesHook = nil })

	p := Pipeline{
		Env:      map[string]string{"FOO": "bar"},
		Services: map[string]Service{"db": {Image: "postgres:16"}, "x": {Image: "y"}},
		Steps:    []Step{{Name: "a", Services: []string{"db"}}},
	}
	teardown, _, err := startServicesForPipeline(context.Background(), p, nil, "/tmp", io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	teardown()
	if _, ok := gotServices["db"]; !ok {
		t.Errorf("expected db in requested, got %v", gotServices)
	}
	if _, ok := gotServices["x"]; ok {
		t.Errorf("expected x filtered out, got %v", gotServices)
	}
	if gotEnv["FOO"] != "bar" {
		t.Errorf("expected FOO=bar in env overlay, got %v", gotEnv)
	}
}
