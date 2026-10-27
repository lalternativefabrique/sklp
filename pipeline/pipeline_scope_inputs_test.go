package pipeline

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/lalternativefabrique/sklp/pipeline/spec"
)

// These tests pin the precedence rule that powers
// "scope: is the source of truth for cache invalidation":
//
//	explicit inputs: > scope-expanded > nothing
//
// They exercise CompilePipeline directly so the test does not depend
// on a real shell / git working tree — the resolver is fed via a
// repo root that contains the expected files.

func TestCompilePipeline_ExplicitInputsWinOverScope(t *testing.T) {
	repo := t.TempDir()
	mustWrite(t, filepath.Join(repo, "go.mod"), "module x\n")
	mustWrite(t, filepath.Join(repo, "apps", "core", "main.go"), "package main\n")

	p := Pipeline{
		Version: 1,
		Kind:    "pipeline",
		Scopes: map[string][]string{
			"backend": {"apps/core/**"},
		},
		Steps: []Step{{
			Name:   "build",
			Run:    "true",
			Scope:  ScopeRef{"backend"},
			Inputs: []string{"go.mod"},
		}},
	}

	pl, err := CompilePipeline(p, CompilePipelineOptions{RepoRoot: repo})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got := pl.CI.Steps[0].ResolvedInputs
	want := []string{"go.mod"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("explicit inputs ignored: got %v, want %v", got, want)
	}
}

func TestCompilePipeline_ScopeBecomesImplicitInputs(t *testing.T) {
	repo := t.TempDir()
	mustWrite(t, filepath.Join(repo, "apps", "core", "main.go"), "package main\n")
	mustWrite(t, filepath.Join(repo, "apps", "core", "util.go"), "package main\n")
	mustWrite(t, filepath.Join(repo, "README.md"), "ignored\n")

	p := Pipeline{
		Version: 1,
		Kind:    "pipeline",
		Scopes: map[string][]string{
			"backend": {"apps/core/**"},
		},
		Steps: []Step{{
			Name:  "build",
			Run:   "true",
			Scope: ScopeRef{"backend"},
		}},
	}

	pl, err := CompilePipeline(p, CompilePipelineOptions{RepoRoot: repo})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got := append([]string(nil), pl.CI.Steps[0].ResolvedInputs...)
	sort.Strings(got)
	want := []string{"apps/core/main.go", "apps/core/util.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scope-implicit inputs wrong: got %v, want %v", got, want)
	}
}

func TestCompilePipeline_NoScopeNoInputsResolvesEmpty(t *testing.T) {
	repo := t.TempDir()
	mustWrite(t, filepath.Join(repo, "apps", "core", "main.go"), "package main\n")

	p := Pipeline{
		Version: 1,
		Kind:    "pipeline",
		Steps: []Step{{
			Name: "neutral",
			Run:  "true",
		}},
	}

	pl, err := CompilePipeline(p, CompilePipelineOptions{RepoRoot: repo})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if got := pl.CI.Steps[0].ResolvedInputs; len(got) != 0 {
		t.Fatalf("expected no resolved inputs, got %v", got)
	}
}

func TestCompilePipeline_MultiScopeUnionsGlobs(t *testing.T) {
	repo := t.TempDir()
	mustWrite(t, filepath.Join(repo, "apps", "core", "main.go"), "go\n")
	mustWrite(t, filepath.Join(repo, "shared", "x.go"), "go\n")

	p := Pipeline{
		Version: 1,
		Kind:    "pipeline",
		Scopes: map[string][]string{
			"backend": {"apps/core/**"},
			"shared":  {"shared/**"},
		},
		Steps: []Step{{
			Name:  "build",
			Run:   "true",
			Scope: ScopeRef{"backend", "shared"},
		}},
	}

	pl, err := CompilePipeline(p, CompilePipelineOptions{RepoRoot: repo})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got := append([]string(nil), pl.CI.Steps[0].ResolvedInputs...)
	sort.Strings(got)
	want := []string{"apps/core/main.go", "shared/x.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("multi-scope union wrong: got %v, want %v", got, want)
	}
}

func TestExpandScopesToGlobs_DedupAndSort(t *testing.T) {
	defs := map[string][]string{
		"a": {"src/**/*.go", "go.mod"},
		"b": {"go.mod", "shared/**"},
	}
	got, err := spec.ExpandScopesToGlobs(defs, []string{"a", "b"})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	want := []string{"go.mod", "shared/**", "src/**/*.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestExpandScopesToGlobs_UnknownScopeErrors(t *testing.T) {
	_, err := spec.ExpandScopesToGlobs(map[string][]string{"a": {"x"}}, []string{"missing"})
	if err == nil {
		t.Fatalf("expected error for unknown scope")
	}
}

// TestRunPipeline_ScopeDrivesCacheInvalidation is the end-to-end
// acceptance for the user-facing fix: a step declaring only `scope:`
// (no explicit `inputs:`) must miss the cache when a file matching
// the scope changes, and must hit the cache when an unrelated file
// changes.
func TestRunPipeline_ScopeDrivesCacheInvalidation(t *testing.T) {
	repo := t.TempDir()
	mustWrite(t, filepath.Join(repo, "src", "x.go"), "package main\n")
	mustWrite(t, filepath.Join(repo, "README.md"), "v1\n")

	yamlPath := filepath.Join(repo, "ci.yaml")
	mustWrite(t, yamlPath, `version: 1
kind: pipeline
name: demo
scopes:
  backend: ["src/**/*.go"]
steps:
  - name: build
    scope: [backend]
    run: echo build
`)

	p, err := LoadPipeline(yamlPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	run := func() PipelineResult {
		t.Helper()
		var stdout, stderr bytes.Buffer
		res, runErr := RunPipeline(context.Background(), p, PipelineRunOptions{
			RepoPath: repo,
			Stdout:   &stdout,
			Stderr:   &stderr,
		})
		if runErr != nil {
			t.Fatalf("run: %v\nstdout=%s\nstderr=%s", runErr, stdout.String(), stderr.String())
		}
		return res
	}

	// First run — populates cache.
	r1 := run()
	if len(r1.Steps) != 1 || r1.Steps[0].Status != "passed" {
		t.Fatalf("first run: got %+v, want one passed step", r1.Steps)
	}

	// Second run, no changes — must serve from cache.
	r2 := run()
	if len(r2.Steps) != 1 || r2.Steps[0].Status != "cached" {
		t.Fatalf("second run (no change): got %+v, want cached", r2.Steps)
	}

	// Edit an UNRELATED file (outside scope) — must still hit cache.
	mustWrite(t, filepath.Join(repo, "README.md"), "v2\n")
	r3 := run()
	if len(r3.Steps) != 1 || r3.Steps[0].Status != "cached" {
		t.Fatalf("third run (out-of-scope change): got %+v, want cached", r3.Steps)
	}

	// Edit a file MATCHED by the scope — must miss the cache. This is
	// the regression: pre-fix the cache key ignored the working tree
	// so this run would silently return "cached".
	mustWrite(t, filepath.Join(repo, "src", "x.go"), "package main\n// changed\n")
	r4 := run()
	if len(r4.Steps) != 1 || r4.Steps[0].Status != "passed" {
		t.Fatalf("fourth run (in-scope change): got %+v, want passed (cache miss)", r4.Steps)
	}
}

// TestRunPipeline_ExplicitInputsBeatScopeAtRuntime verifies that when
// both `inputs:` and `scope:` are declared, only `inputs:` drives the
// cache key: a change to an in-scope-but-not-in-inputs file keeps the
// cache hit.
func TestRunPipeline_ExplicitInputsBeatScopeAtRuntime(t *testing.T) {
	repo := t.TempDir()
	mustWrite(t, filepath.Join(repo, "go.mod"), "module x\n")
	mustWrite(t, filepath.Join(repo, "src", "x.go"), "package main\n")

	yamlPath := filepath.Join(repo, "ci.yaml")
	mustWrite(t, yamlPath, `version: 1
kind: pipeline
name: demo
scopes:
  backend: ["src/**/*.go"]
steps:
  - name: build
    scope: [backend]
    inputs: ["go.mod"]
    run: echo build
`)

	p, err := LoadPipeline(yamlPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	run := func() string {
		t.Helper()
		var sout, serr bytes.Buffer
		res, err := RunPipeline(context.Background(), p, PipelineRunOptions{
			RepoPath: repo, Stdout: &sout, Stderr: &serr,
		})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		return res.Steps[0].Status
	}

	if s := run(); s != "passed" {
		t.Fatalf("first run: status=%q want passed", s)
	}
	// Change a file IN scope but NOT in inputs — explicit wins → cached.
	mustWrite(t, filepath.Join(repo, "src", "x.go"), "package main\n// drift\n")
	if s := run(); s != "cached" {
		t.Fatalf("explicit inputs ignored at runtime: status=%q want cached", s)
	}
	// Change the file that IS in explicit inputs → cache miss.
	mustWrite(t, filepath.Join(repo, "go.mod"), "module x\n// changed\n")
	if s := run(); s != "passed" {
		t.Fatalf("explicit input change ignored: status=%q want passed", s)
	}
}

// TestResolveInputGlobs_SymlinkLoopsAndStoresPruned pins the govulncheck-core
// ELOOP fix: pnpm's .pnpm-store contains a symlink cycling back to the repo
// root; `**` expansion must neither follow symlinks nor descend into artefact
// stores, and vendored sources under node_modules must not key the cache.
func TestResolveInputGlobs_SymlinkLoopsAndStoresPruned(t *testing.T) {
	repo := t.TempDir()
	mustWrite(t, filepath.Join(repo, "apps", "core", "api.go"), "package core\n")
	mustWrite(t, filepath.Join(repo, "node_modules", "pkg", "vendored.go"), "package pkg\n")

	// pnpm-style circular symlink: .pnpm-store/v11/links/loop -> repo root.
	storeDir := filepath.Join(repo, ".pnpm-store", "v11", "links")
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		t.Fatalf("mkdir store: %v", err)
	}
	if err := os.Symlink("../../..", filepath.Join(storeDir, "loop")); err != nil {
		t.Fatalf("symlink store loop: %v", err)
	}
	// Self-referencing symlink OUTSIDE any pruned dir — noFollow must cover it.
	if err := os.Symlink(".", filepath.Join(repo, "self")); err != nil {
		t.Fatalf("symlink self: %v", err)
	}

	got, err := spec.ResolveInputGlobs(repo, []string{"**/*.go"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := []string{"apps/core/api.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	// Literal paths bypass ReadDir pruning: explicitly declared inputs under a
	// pruned dir still resolve.
	got, err = spec.ResolveInputGlobs(repo, []string{"node_modules/pkg/vendored.go"})
	if err != nil {
		t.Fatalf("resolve literal: %v", err)
	}
	want = []string{"node_modules/pkg/vendored.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("literal under pruned dir: got %v, want %v", got, want)
	}
}

func mustWrite(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}
