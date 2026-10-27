package pipeline

import (
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/plan"
)

// TestCompilePipeline_BuildContexts_LocalDirAppendedToInputs is the
// cache-invalidation regression: a `kind: docker-build` step with a
// `build_contexts:` entry pointing at a local directory must have that
// directory folded into the step's Inputs so an edit there busts the
// cache. Image / target: refs flow through the spec instead — they
// must NOT appear in Inputs.
func TestCompilePipeline_BuildContexts_LocalDirAppendedToInputs(t *testing.T) {
	p := Pipeline{
		Steps: []Step{{
			Name:       "web",
			Kind:       "publish",
			Image:      "registry.example.com/web",
			Tag:        "v1",
			Context:    ".",
			Dockerfile: "Dockerfile",
			BuildContexts: map[string]string{
				"platform":  "vendor/platform",    // in-repo local-dir → Inputs
				"schemas":   "docker-image://x:1", // remote → NOT in Inputs
				"api-stage": "target:api",         // stage → NOT in Inputs
				"upstream":  "../outside",         // out-of-repo → spec only
			},
		}},
	}

	compiled, err := CompilePipeline(p, CompilePipelineOptions{
		RepoRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(compiled.CI.Steps) != 1 {
		t.Fatalf("expected 1 compiled step, got %d", len(compiled.CI.Steps))
	}
	got := compiled.CI.Steps[0].Inputs

	// In-repo local-dir sources are folded into Inputs (Merkle hash);
	// remote refs + out-of-repo paths flow only through the spec.
	wantContains := []string{"vendor/platform/**"}
	wantAbsent := []string{
		"docker-image://x:1/**", // image must not pollute Inputs
		"target:api/**",         // stage either
		"../outside/**",         // out-of-repo path skipped
	}
	joined := strings.Join(got, " | ")
	for _, w := range wantContains {
		if !strings.Contains(joined, w) {
			t.Errorf("Inputs missing %q\nactual: %v", w, got)
		}
	}
	for _, w := range wantAbsent {
		if strings.Contains(joined, w) {
			t.Errorf("Inputs unexpectedly contains %q (should be hashed via spec, not Inputs)\nactual: %v",
				w, got)
		}
	}

	// The spec itself carries every entry — that's what the cache key
	// uses for the remote refs.
	spec := compiled.CI.Steps[0].Publish
	if spec == nil {
		t.Fatal("Publish spec is nil")
	}
	if got, want := len(spec.BuildContexts), 4; got != want {
		t.Errorf("spec.BuildContexts len = %d, want %d", got, want)
	}
}

// TestCompilePipeline_BuildContexts_ExplicitInputsRespected guards the
// user's explicit declaration: when Inputs is non-empty, the runner
// must NOT auto-derive from build_contexts — it would silently widen a
// careful cache hint.
func TestCompilePipeline_BuildContexts_ExplicitInputsRespected(t *testing.T) {
	p := Pipeline{
		Steps: []Step{{
			Name:       "web",
			Kind:       "publish",
			Image:      "registry.example.com/web",
			Tag:        "v1",
			Context:    ".",
			Dockerfile: "Dockerfile",
			Inputs:     []string{"Dockerfile", "src/**"}, // user-curated
			BuildContexts: map[string]string{
				"platform": "../platform",
			},
		}},
	}
	compiled, err := CompilePipeline(p, CompilePipelineOptions{
		RepoRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got := compiled.CI.Steps[0].Inputs
	for _, ent := range got {
		if strings.Contains(ent, "../platform") {
			t.Errorf("explicit Inputs widened by build_contexts (auto-derivation should be gated): %v", got)
		}
	}
}

// TestCompilePipeline_BuildContexts_OnlyAppliesToPublish is a
// belt-and-braces check: the helper plumbing must not leak into shell
// steps (BuildContexts is yaml-typed on every Step but only meaningful
// for kind: docker-build).
func TestCompilePipeline_BuildContexts_OnlyAppliesToPublish(t *testing.T) {
	p := Pipeline{
		Steps: []Step{{
			Name:          "lint",
			Run:           "echo ok",
			BuildContexts: map[string]string{"platform": "../platform"},
		}},
	}
	compiled, err := CompilePipeline(p, CompilePipelineOptions{
		RepoRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if compiled.CI.Steps[0].StepKind == plan.KindCIStepPublish {
		t.Fatalf("shell step misclassified")
	}
	// No auto-derivation should have happened on a shell step.
	for _, ent := range compiled.CI.Steps[0].Inputs {
		if strings.Contains(ent, "../platform") {
			t.Errorf("shell step picked up build_contexts auto-derivation: %v", compiled.CI.Steps[0].Inputs)
		}
	}
}
