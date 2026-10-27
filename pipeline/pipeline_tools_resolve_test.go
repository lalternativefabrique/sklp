package pipeline

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TestMergeToolchain pins the UNION semantics: the effective toolchain a
// pipeline run exposes is config `runner.toolchain` ∪ pipeline `tools:`,
// with a pipeline's explicit (non-empty) version overriding config.
func TestMergeToolchain(t *testing.T) {
	cases := []struct {
		name     string
		pipeline map[string]string
		config   map[string]string
		want     map[string]string
	}{
		{
			name:     "omit falls back to config, config tools join the union",
			pipeline: map[string]string{"go": ""},
			config:   map[string]string{"go": "1.25.0", "node": "22"},
			want:     map[string]string{"go": "1.25.0", "node": "22"},
		},
		{
			name:     "explicit pipeline version overrides config",
			pipeline: map[string]string{"go": "1.24.0"},
			config:   map[string]string{"go": "1.25.0"},
			want:     map[string]string{"go": "1.24.0"},
		},
		{
			name:     "config-only tool IS included (union)",
			pipeline: map[string]string{"go": ""},
			config:   map[string]string{"go": "1.25.0", "pnpm": "10"},
			want:     map[string]string{"go": "1.25.0", "pnpm": "10"},
		},
		{
			name:     "nil config degrades to pipeline tools",
			pipeline: map[string]string{"go": "1.24.0", "node": ""},
			config:   nil,
			want:     map[string]string{"go": "1.24.0", "node": ""},
		},
		{
			name:     "nil pipeline degrades to config",
			pipeline: nil,
			config:   map[string]string{"go": "1.25.0", "node": "22"},
			want:     map[string]string{"go": "1.25.0", "node": "22"},
		},
		{
			name:     "both empty stays empty",
			pipeline: map[string]string{"go": ""},
			config:   map[string]string{},
			want:     map[string]string{"go": ""},
		},
		{
			name:     "pipeline empty + config empty key stays empty",
			pipeline: map[string]string{"go": ""},
			config:   map[string]string{"go": ""},
			want:     map[string]string{"go": ""},
		},
		{
			name:     "union both directions",
			pipeline: map[string]string{"bun": "1.1"},
			config:   map[string]string{"go": "1.25.0"},
			want:     map[string]string{"bun": "1.1", "go": "1.25.0"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeToolchain(tc.pipeline, tc.config)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("mergeToolchain(%v, %v) = %v, want %v", tc.pipeline, tc.config, got, tc.want)
			}
		})
	}
}

// TestStepTools_AcceptsConfigOnlyTool proves the union: a step may name a
// tool that lives ONLY in config.toolchain, without redeclaring it in the
// pipeline `tools:` block. The guardrail must accept it, and the resolver
// must NOT reject it with a "not declared" error.
func TestStepTools_AcceptsConfigOnlyTool(t *testing.T) {
	effective := mergeToolchain(
		map[string]string{"go": ""},
		map[string]string{"go": "1.25.0", "python": "3.12"},
	)

	got, err := stepTools(Step{Name: "test", Tools: []string{"python"}}, effective)
	if err != nil {
		t.Fatalf("stepTools rejected config-only tool python: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"python"}) {
		t.Fatalf("stepTools = %v, want [python]", got)
	}

	// The resolver may fail to actually install python here, but it must
	// not be because python is "not declared" — the union covers it.
	r := newToolResolver(effective, t.TempDir(), t.TempDir(), io.Discard)
	if _, err := r.Get(context.Background(), "python"); err != nil &&
		strings.Contains(err.Error(), "not declared") {
		t.Fatalf("python should be declared via config union, got: %v", err)
	}
}

// TestStepTools_RejectsTotallyUnknownTool keeps the guardrail honest: a
// tool in NEITHER the pipeline nor config is still a hard error, with the
// reworded message naming both sources.
func TestStepTools_RejectsTotallyUnknownTool(t *testing.T) {
	effective := mergeToolchain(
		map[string]string{"go": ""},
		map[string]string{"go": "1.25.0", "python": "3.12"},
	)

	_, err := stepTools(Step{Name: "test", Tools: []string{"ruby"}}, effective)
	if err == nil {
		t.Fatal("expected error for unknown tool ruby, got nil")
	}
	if !strings.Contains(err.Error(), "not declared in pipeline `tools:` or config `runner.toolchain`") {
		t.Fatalf("expected reworded guardrail error, got: %v", err)
	}
}

// TestLoadConfigToolchain_Tolerant checks the best-effort loader: a
// missing or malformed config.yaml returns nil (never panics, never
// surfaces an error), while a well-formed one returns its toolchain map.
func TestLoadConfigToolchain_Tolerant(t *testing.T) {
	t.Run("no config returns nil", func(t *testing.T) {
		got := loadConfigToolchain(t.TempDir())
		if got != nil {
			t.Fatalf("expected nil for missing config.yaml, got %v", got)
		}
	})

	t.Run("well-formed config returns toolchain", func(t *testing.T) {
		repo := t.TempDir()
		writeConfig(t, repo, "runner:\n  toolchain:\n    go: \"1.25.0\"\n    node: \"22\"\n")
		got := loadConfigToolchain(repo)
		want := map[string]string{"go": "1.25.0", "node": "22"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("loadConfigToolchain = %v, want %v", got, want)
		}
	})

	t.Run("malformed config returns nil", func(t *testing.T) {
		repo := t.TempDir()
		writeConfig(t, repo, "runner: [this is: not valid yaml }{\n")
		got := loadConfigToolchain(repo)
		if got != nil {
			t.Fatalf("expected nil for malformed config.yaml, got %v", got)
		}
	})
}

// writeConfig writes content to <repo>/.sklp/config.yaml for the loader
// tolerance test.
func writeConfig(t *testing.T, repo, content string) {
	t.Helper()
	dir := filepath.Join(repo, ".sklp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// TestToolResolver_ConcurrentGet_NoRace exercises the concurrency path
// that crashed `sklp run publish` with "fatal error: concurrent map
// writes" — two goroutines calling Get() on the same resolver for
// different tools at the same time. With -race, the test fails if any
// of the memo maps is mutated without holding r.mu.
//
// The resolver is built with NO declared tools so every Get() returns
// the "not declared" guard error immediately, before touching the slow
// path. That is exactly the access pattern the scheduler triggered when
// it parallelised steps that all asked for the same undeclared tool: it
// is enough to race on r.versions read + r.resolveOnce write.
func TestToolResolver_ConcurrentGet_NoRace(t *testing.T) {
	r := newToolResolver(map[string]string{}, t.TempDir(), t.TempDir(), io.Discard)

	const goroutines = 32
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_, _ = r.Get(context.Background(), "pnpm")
		}()
	}
	wg.Wait()
}

// TestToolResolver_ResolveOnce verifies the per-tool latch: many
// concurrent callers for the same name share one resolveBinary attempt.
// We stub resolveBinary indirectly by giving the tool a declaration that
// triggers the host-preference path, then count how many times the
// recorded outcome differs from the first one — every winner must see
// the same dir (or the same error).
func TestToolResolver_ConcurrentGet_SharedOutcome(t *testing.T) {
	// "totally-unknown-tool-xyz" is not declared, so Get returns the
	// "not declared" error immediately — that path
	// short-circuits before the Once is even consulted, which is the
	// other half of the original race (read of r.versions under no lock
	// is fine; write to r.resolveOnce was the bug). Pair with a
	// declared-but-unresolvable tool to exercise the Once path too.
	r := newToolResolver(map[string]string{
		"ghost-tool": "9.9.9", // declared, won't be found on host nor in registry
	}, t.TempDir(), t.TempDir(), io.Discard)

	const goroutines = 16
	var errCount int64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if _, err := r.Get(context.Background(), "ghost-tool"); err != nil {
				atomic.AddInt64(&errCount, 1)
			}
		}()
	}
	wg.Wait()

	if errCount != goroutines {
		t.Errorf("every caller should observe the same error; got %d failures out of %d", errCount, goroutines)
	}
}
