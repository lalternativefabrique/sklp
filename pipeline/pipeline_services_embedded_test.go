package pipeline

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

// TestPipelineServicesForEmbedded_Maps verifies the host-side mapping:
// union of step.Services (no scope filter), Optional dropped, env merged.
func TestPipelineServicesForEmbedded_Maps(t *testing.T) {
	p := Pipeline{
		Env: map[string]string{"GLOBAL": "1", "OVERRIDE": "pipeline"},
		Services: map[string]Service{
			"db": {
				Image:     "postgres:16",
				Env:       map[string]string{"OVERRIDE": "service", "PGDATA": "/data"},
				DependsOn: []string{"migrate"},
				Ready:     &Ready{TCP: "localhost:5432", Timeout: "20s"},
				Ports:     []string{"5432:5432"},
			},
			"migrate": {Image: "migrate:1", Oneshot: true},
			"opt":     {Image: "extra:1", Optional: true},
			"unused":  {Image: "busybox"},
		},
		Steps: []Step{
			{Name: "a", Services: []string{"db"}},
			{Name: "b", Services: []string{"migrate", "opt"}},
		},
	}

	got := pipelineServicesForEmbedded(p)

	// db + migrate requested; opt is Optional (dropped); unused not referenced.
	if len(got) != 2 {
		t.Fatalf("expected 2 services (db, migrate), got %d: %+v", len(got), got)
	}

	byName := map[string]int{}
	for i, s := range got {
		byName[s.Name] = i
	}
	db, ok := byName["db"]
	if !ok {
		t.Fatal("missing db")
	}
	if _, ok := byName["migrate"]; !ok {
		t.Fatal("missing migrate")
	}
	if _, ok := byName["opt"]; ok {
		t.Fatal("Optional service must be dropped")
	}

	s := got[db]
	if s.Image != "postgres:16" {
		t.Errorf("image: got %q", s.Image)
	}
	// service Env wins over pipeline Env on collision; pipeline keys merged.
	if s.Env["OVERRIDE"] != "service" {
		t.Errorf("service Env must win: got OVERRIDE=%q", s.Env["OVERRIDE"])
	}
	if s.Env["GLOBAL"] != "1" {
		t.Errorf("pipeline Env must merge in: got GLOBAL=%q", s.Env["GLOBAL"])
	}
	if s.Env["PGDATA"] != "/data" {
		t.Errorf("service Env missing: got PGDATA=%q", s.Env["PGDATA"])
	}
	if s.Ready == nil || s.Ready.TCP != "localhost:5432" || s.Ready.Timeout != "20s" {
		t.Errorf("ready not mapped: %+v", s.Ready)
	}
	if len(s.DependsOn) != 1 || s.DependsOn[0] != "migrate" {
		t.Errorf("depends_on not mapped: %v", s.DependsOn)
	}
}

// TestPipelineServicesForEmbedded_NoServices returns nil so the runtime
// stays on its single-container path.
func TestPipelineServicesForEmbedded_NoServices(t *testing.T) {
	p := Pipeline{Steps: []Step{{Name: "only", Run: "true"}}}
	if got := pipelineServicesForEmbedded(p); got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

// TestStartServicesForPipeline_SkipsUnderEmbedded confirms the embedded
// contract: the orchestrator hook is bypassed AND no port-env map is
// published — step env addresses each service as <name>:<port> verbatim.
func TestStartServicesForPipeline_SkipsUnderEmbedded(t *testing.T) {
	t.Setenv(RunnerModeEnv, RunnerModeEmbedded)

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
		Services: map[string]Service{"db": {Image: "postgres:16", Ports: []string{"5432:5432"}}},
		Steps:    []Step{{Name: "a", Services: []string{"db"}}},
	}
	var stderr bytes.Buffer
	teardown, portEnv, err := startServicesForPipeline(context.Background(), p, nil, "/tmp", io.Discard, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	teardown()
	if called {
		t.Fatal("hook must not be called under embedded mode")
	}
	if !strings.Contains(stderr.String(), "services managed externally") {
		t.Errorf("expected external-services skip log, got: %q", stderr.String())
	}
	// No port-env published under embedded mode: services are addressed
	// verbatim as <name>:<port> via spacenet DNS.
	if len(portEnv) != 0 {
		t.Errorf("expected empty port-env, got %v", portEnv)
	}
}

// TestServicesManagedExternally covers the mode matrix.
func TestServicesManagedExternally(t *testing.T) {
	cases := []struct {
		mode string
		want bool
	}{
		{RunnerModeK8s, true},
		{RunnerModeEmbedded, true},
		{"", false},
		{"something-else", false},
	}
	for _, c := range cases {
		t.Setenv(RunnerModeEnv, c.mode)
		if got := servicesManagedExternally(); got != c.want {
			t.Errorf("mode=%q: got %v want %v", c.mode, got, c.want)
		}
	}
}
