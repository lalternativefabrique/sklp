package pipeline

import (
	"testing"

	"github.com/lalternativefabrique/sklp/internal/plan"
)

// TestCompilePipeline_Force_BypassesScopeFilter pins the -force semantics
// for kind: pipeline: every scoped step must compile to StatusIncluded
// regardless of the ChangedFiles diff, mirroring the legacy kind: task
// applyForce behaviour. Without Force, the same input must skip every
// step with "no changes in scope".
func TestCompilePipeline_Force_BypassesScopeFilter(t *testing.T) {
	p := Pipeline{
		Version: 1,
		Kind:    "pipeline",
		Scopes: map[string][]string{
			"backend":  {"services/api/**"},
			"frontend": {"apps/web/**"},
			"sdk":      {"packages/sdk/**"},
		},
		Steps: []Step{
			{Name: "test-core", Run: "true", Scope: ScopeRef{"backend"}},
			{Name: "test-web", Run: "true", Scope: ScopeRef{"frontend"}},
			{Name: "test-sdk", Run: "true", Scope: ScopeRef{"sdk"}},
		},
	}

	// docs/README.md does not match any scope glob. The non-nil
	// ChangedFiles bypasses the "empty diff → run anyway" degraded-open
	// branch in scopeMatches, so every step would normally be skipped.
	changed := []string{"docs/README.md"}

	t.Run("without Force: all steps skipped", func(t *testing.T) {
		pl, err := CompilePipeline(p, CompilePipelineOptions{
			ChangedFiles: changed,
		})
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		for _, s := range pl.CI.Steps {
			if s.Status != plan.StatusSkippedCompile {
				t.Errorf("step %q: status=%v want StatusSkippedCompile (reason=%q)", s.Name, s.Status, s.SkipReason)
			}
		}
	})

	t.Run("with Force: all steps included", func(t *testing.T) {
		pl, err := CompilePipeline(p, CompilePipelineOptions{
			ChangedFiles: changed,
			Force:        true,
		})
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		if got, want := len(pl.CI.Steps), 3; got != want {
			t.Fatalf("compiled %d steps, want %d", got, want)
		}
		for _, s := range pl.CI.Steps {
			if s.Status != plan.StatusIncluded {
				t.Errorf("step %q: status=%v want StatusIncluded (reason=%q)", s.Name, s.Status, s.SkipReason)
			}
		}
	})
}
