package pipeline

import (
	"bytes"
	"strings"
	"testing"
)

func TestCheckScopeOutputRecursion(t *testing.T) {
	cases := []struct {
		name         string
		pipeline     Pipeline
		taskPath     string
		wantContains []string
		wantEmpty    bool
	}{
		{
			name: "no scopes, no warning",
			pipeline: Pipeline{
				Steps: []Step{{Name: "build", Outputs: []string{"out/a.bin"}}},
			},
			taskPath:  ".sklp/tasks/build.yaml",
			wantEmpty: true,
		},
		{
			name: "scopes but no declared outputs",
			pipeline: Pipeline{
				Scopes: map[string][]string{"schema": {"docs/sklp/pipeline.schema.json"}},
				Steps: []Step{
					{Name: "regen", Run: "go generate ./..."},
				},
			},
			taskPath:  ".sklp/tasks/regen-schemas.yaml",
			wantEmpty: true,
		},
		{
			name: "no overlap, no warning",
			pipeline: Pipeline{
				Scopes: map[string][]string{"src": {"apps/core/**"}},
				Steps: []Step{
					{Name: "build", Outputs: []string{"dist/server"}},
				},
			},
			taskPath:  ".sklp/tasks/ci.yaml",
			wantEmpty: true,
		},
		{
			name: "exact match triggers warning",
			pipeline: Pipeline{
				Scopes: map[string][]string{"schema": {"docs/sklp/pipeline.schema.json"}},
				Steps: []Step{
					{Name: "regen", Outputs: []string{"docs/sklp/pipeline.schema.json"}},
				},
			},
			taskPath: ".sklp/tasks/regen-schemas.yaml",
			wantContains: []string{
				"warning: scope-output recursion risk in .sklp/tasks/regen-schemas.yaml",
				"step \"regen\" writes output \"docs/sklp/pipeline.schema.json\"",
				"which matches scope \"schema\" glob \"docs/sklp/pipeline.schema.json\"",
				"infinite trigger loop",
				"remove the generated file from the scope",
			},
		},
		{
			name: "doublestar match triggers warning",
			pipeline: Pipeline{
				Scopes: map[string][]string{"web": {"build/**"}},
				Steps: []Step{
					{Name: "bundle", Outputs: []string{"build/web.js"}},
				},
			},
			taskPath: ".sklp/tasks/web.yaml",
			wantContains: []string{
				"step \"bundle\" writes output \"build/web.js\"",
				"which matches scope \"web\" glob \"build/**\"",
			},
		},
		{
			name: "multiple offending pairs, one warning each, deterministic order",
			pipeline: Pipeline{
				Scopes: map[string][]string{
					"schema": {"docs/**"},
					"types":  {"types/**"},
				},
				Steps: []Step{
					{Name: "regen-b", Outputs: []string{"types/api.d.ts"}},
					{Name: "regen-a", Outputs: []string{"docs/z.json", "docs/a.json"}},
				},
			},
			taskPath: ".sklp/tasks/regen.yaml",
			wantContains: []string{
				// Sorted by step name then output path. We assert the
				// relative order via a substring sequence below.
				"step \"regen-a\" writes output \"docs/a.json\"",
				"step \"regen-a\" writes output \"docs/z.json\"",
				"step \"regen-b\" writes output \"types/api.d.ts\"",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			checkScopeOutputRecursion(tc.pipeline, tc.taskPath, &buf)
			got := buf.String()
			if tc.wantEmpty {
				if got != "" {
					t.Fatalf("expected no warning, got:\n%s", got)
				}
				return
			}
			// Assert the listed substrings appear in order.
			prev := 0
			for _, want := range tc.wantContains {
				idx := strings.Index(got[prev:], want)
				if idx < 0 {
					t.Fatalf("warning missing %q (or out of order)\nfull output:\n%s", want, got)
				}
				prev += idx + len(want)
			}
		})
	}
}

func TestCheckScopeOutputRecursion_NilWriter(t *testing.T) {
	// Must not panic when Warnings is nil.
	checkScopeOutputRecursion(Pipeline{
		Scopes: map[string][]string{"x": {"a"}},
		Steps:  []Step{{Name: "s", Outputs: []string{"a"}}},
	}, "t.yaml", nil)
}
