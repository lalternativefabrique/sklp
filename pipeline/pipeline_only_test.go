package pipeline

import (
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/plan"
)

func onlyTestPipeline() Pipeline {
	return Pipeline{
		Version: 1,
		Kind:    "pipeline",
		Scopes: map[string][]string{
			"core":    {"services/api/**"},
			"web":     {"apps/web/**"},
			"migrate": {"apps/migrations/**"},
		},
		Steps: []Step{
			{Name: "core-image", Run: "true", Scope: ScopeRef{"core"}},
			{Name: "web-image", Run: "true", Scope: ScopeRef{"web"}},
			{Name: "migrate-image", Run: "true", Scope: ScopeRef{"migrate"}},
		},
	}
}

func statusByStep(t *testing.T, pl *plan.Plan) map[string]plan.CIStepStatus {
	t.Helper()
	out := make(map[string]plan.CIStepStatus, len(pl.CI.Steps))
	for _, s := range pl.CI.Steps {
		out[s.Name] = s.Status
	}
	return out
}

// TestCompilePipeline_Only_SelectsNamedScopes pins the core -only contract:
// exactly the steps naming a selected scope are included, every other step is
// skipped — and the selection holds even when the diff touches nothing, which
// is the whole point of republishing one service from a clean tree.
func TestCompilePipeline_Only_SelectsNamedScopes(t *testing.T) {
	p := onlyTestPipeline()
	changed := []string{"services/api/main.go"}

	t.Run("single scope", func(t *testing.T) {
		pl, err := CompilePipeline(p, CompilePipelineOptions{
			ChangedFiles: changed,
			Only:         []string{"web"},
		})
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		got := statusByStep(t, pl)
		if got["web-image"] != plan.StatusIncluded {
			t.Errorf("web-image: status=%v want StatusIncluded", got["web-image"])
		}
		// core matches the diff but was not selected: -only narrows, and a
		// diff hit must not smuggle a step back into the run.
		for _, name := range []string{"core-image", "migrate-image"} {
			if got[name] != plan.StatusSkippedCompile {
				t.Errorf("%s: status=%v want StatusSkippedCompile", name, got[name])
			}
		}
	})

	t.Run("multiple scopes", func(t *testing.T) {
		pl, err := CompilePipeline(p, CompilePipelineOptions{
			ChangedFiles: changed,
			Only:         []string{"web", "migrate"},
		})
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		got := statusByStep(t, pl)
		for _, name := range []string{"web-image", "migrate-image"} {
			if got[name] != plan.StatusIncluded {
				t.Errorf("%s: status=%v want StatusIncluded", name, got[name])
			}
		}
		if got["core-image"] != plan.StatusSkippedCompile {
			t.Errorf("core-image: status=%v want StatusSkippedCompile", got["core-image"])
		}
	})

	t.Run("selection ignores an unrelated diff", func(t *testing.T) {
		pl, err := CompilePipeline(p, CompilePipelineOptions{
			ChangedFiles: []string{"docs/README.md"},
			Only:         []string{"core"},
		})
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		if got := statusByStep(t, pl); got["core-image"] != plan.StatusIncluded {
			t.Errorf("core-image: status=%v want StatusIncluded", got["core-image"])
		}
	})
}

// TestCompilePipeline_Only_OverridesForce pins the precedence: -only narrows,
// -force widens, and narrowing wins. Otherwise `-force -only web` would
// publish every image while the operator asked for exactly one.
func TestCompilePipeline_Only_OverridesForce(t *testing.T) {
	pl, err := CompilePipeline(onlyTestPipeline(), CompilePipelineOptions{
		ChangedFiles: []string{"docs/README.md"},
		Force:        true,
		Only:         []string{"web"},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got := statusByStep(t, pl)
	if got["web-image"] != plan.StatusIncluded {
		t.Errorf("web-image: status=%v want StatusIncluded", got["web-image"])
	}
	for _, name := range []string{"core-image", "migrate-image"} {
		if got[name] != plan.StatusSkippedCompile {
			t.Errorf("%s: status=%v want StatusSkippedCompile (force must not widen past -only)", name, got[name])
		}
	}
}

// TestCompilePipeline_Only_UnscopedStepNeverRuns pins that a step with no
// `scope:` is not selectable: it names nothing, so no -only value can ask
// for it. Running it anyway would contradict the flag.
func TestCompilePipeline_Only_UnscopedStepNeverRuns(t *testing.T) {
	p := onlyTestPipeline()
	p.Steps = append(p.Steps, Step{Name: "always", Run: "true"})

	pl, err := CompilePipeline(p, CompilePipelineOptions{
		ChangedFiles: []string{"apps/web/app.tsx"},
		Only:         []string{"web"},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if got := statusByStep(t, pl); got["always"] != plan.StatusSkippedCompile {
		t.Errorf("always: status=%v want StatusSkippedCompile", got["always"])
	}
}

// TestCompilePipeline_Only_UnknownScopeFails pins the loud failure: a typo
// must not compile into a green run that published nothing at all.
func TestCompilePipeline_Only_UnknownScopeFails(t *testing.T) {
	_, err := CompilePipeline(onlyTestPipeline(), CompilePipelineOptions{
		ChangedFiles: []string{"apps/web/app.tsx"},
		Only:         []string{"wbe"},
	})
	if err == nil {
		t.Fatal("compile succeeded, want an error naming the unknown scope")
	}
	msg := err.Error()
	if !strings.Contains(msg, "wbe") {
		t.Errorf("error %q does not name the unknown scope", msg)
	}
	// The message must list the valid names, so the operator can fix the
	// typo without opening the YAML.
	for _, want := range []string{"core", "web", "migrate"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not list declared scope %q", msg, want)
		}
	}
}

func TestParseOnlyList(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"web", []string{"web"}},
		{"web,core", []string{"web", "core"}},
		{" web , core ", []string{"web", "core"}},
		{"web,,core,", []string{"web", "core"}},
	}
	for _, tc := range cases {
		got := parseOnlyList(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("parseOnlyList(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("parseOnlyList(%q) = %v, want %v", tc.in, got, tc.want)
				break
			}
		}
	}
}
