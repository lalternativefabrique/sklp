package pipeline

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeFiles materialises a map of relative-path → content into a fresh
// temp dir and returns that dir. Used by every test in this file to
// pin its own .sklp/tasks/ shape.
func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	return dir
}

func TestLoadPipeline_NoExtends(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"ci.yaml": `
version: 1
kind: pipeline
runner: base
tools:
  go: "1.25.0"
env:
  CI: "true"
steps:
  - name: test
    run: go test ./...
`,
	})

	p, err := LoadPipeline(filepath.Join(dir, "ci.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if p.Runner != "base" {
		t.Errorf("runner = %q, want base", p.Runner)
	}
	if p.Tools["go"] != "1.25.0" {
		t.Errorf("tools.go = %q, want 1.25.0", p.Tools["go"])
	}
	if p.Env["CI"] != "true" {
		t.Errorf("env.CI = %q, want true", p.Env["CI"])
	}
	if len(p.Steps) != 1 || p.Steps[0].Name != "test" {
		t.Errorf("steps = %+v", p.Steps)
	}
}

func TestLoadPipeline_ExtendsScalar(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"_base.yaml": `
version: 1
runner: base
tools:
  go: "1.25.0"
env:
  CI: "true"
secrets:
  - GITHUB_TOKEN
`,
		"ci.yaml": `
version: 1
kind: pipeline
extends: _base
tools:
  pnpm: "9.0.0"
steps:
  - name: test
    run: go test ./...
`,
	})

	p, err := LoadPipeline(filepath.Join(dir, "ci.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Inherited from _base.
	if p.Runner != "base" {
		t.Errorf("inherited runner = %q, want base", p.Runner)
	}
	if p.Env["CI"] != "true" {
		t.Errorf("inherited env.CI = %q", p.Env["CI"])
	}
	if got := p.Secrets; len(got) != 1 || got[0] != "GITHUB_TOKEN" {
		t.Errorf("inherited secrets = %v", got)
	}
	// Merged tools — both go (from _base) and pnpm (from ci) must be present.
	if p.Tools["go"] != "1.25.0" || p.Tools["pnpm"] != "9.0.0" {
		t.Errorf("tools merge wrong: %+v", p.Tools)
	}
	if len(p.Steps) != 1 {
		t.Errorf("own steps replaced inherited: got %d", len(p.Steps))
	}
	// Extends is cleared on the resolved output.
	if len(p.Extends) != 0 {
		t.Errorf("extends not cleared: %v", p.Extends)
	}
}

func TestLoadPipeline_ExtendsList_LeftToRight(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"_a.yaml": `
version: 1
runner: base
tools: { go: "1.25.0" }
env:   { LAYER: "a" }
`,
		"_b.yaml": `
version: 1
runner: ops
env: { LAYER: "b", EXTRA: "yes" }
`,
		"ci.yaml": `
version: 1
kind: pipeline
extends: [_a, _b]
env: { CI: "true" }
steps:
  - name: noop
    run: "true"
`,
	})

	p, err := LoadPipeline(filepath.Join(dir, "ci.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// _b wins on runner (later in list).
	if p.Runner != "ops" {
		t.Errorf("runner = %q, want ops (b overrides a)", p.Runner)
	}
	// _a contributes go, _b doesn't override → go must persist.
	if p.Tools["go"] != "1.25.0" {
		t.Errorf("tools.go lost: %+v", p.Tools)
	}
	// _b overrides LAYER on top of _a; ci adds CI and inherits EXTRA.
	if p.Env["LAYER"] != "b" || p.Env["EXTRA"] != "yes" || p.Env["CI"] != "true" {
		t.Errorf("env merge wrong: %+v", p.Env)
	}
}

func TestLoadPipeline_ExtendsCycleRejected(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"_a.yaml": `version: 1
extends: _b
`,
		"_b.yaml": `version: 1
extends: _a
`,
		"ci.yaml": `version: 1
kind: pipeline
extends: _a
steps: [{ name: x, run: "true" }]
`,
	})

	_, err := LoadPipeline(filepath.Join(dir, "ci.yaml"))
	if err == nil {
		t.Fatal("expected cycle error, got nil")
	}
}

func TestLoadPipeline_TemplateNotRunnable(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"_base.yaml": `version: 1
runner: base
`,
	})
	_, err := LoadPipeline(filepath.Join(dir, "_base.yaml"))
	if !errors.Is(err, ErrTemplateNotRunnable) {
		t.Fatalf("expected ErrTemplateNotRunnable, got %v", err)
	}
}

func TestLoadPipeline_SecretsConcatDedup(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"_base.yaml": `version: 1
secrets: [GITHUB_TOKEN, NPM_TOKEN]
`,
		"ci.yaml": `version: 1
kind: pipeline
extends: _base
secrets: [NPM_TOKEN, KUBECONFIG_OVH_V1]
steps: [{ name: x, run: "true" }]
`,
	})
	p, err := LoadPipeline(filepath.Join(dir, "ci.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := []string{"GITHUB_TOKEN", "NPM_TOKEN", "KUBECONFIG_OVH_V1"}
	if len(p.Secrets) != len(want) {
		t.Fatalf("secrets = %v, want %v", p.Secrets, want)
	}
	for i, s := range want {
		if p.Secrets[i] != s {
			t.Errorf("secrets[%d] = %q, want %q", i, p.Secrets[i], s)
		}
	}
}

func TestLoadPipeline_CacheDedupByPath(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"_base.yaml": `version: 1
cache:
  - { path: "~/.cache/go-build", key: "go-build-base" }
`,
		"ci.yaml": `version: 1
kind: pipeline
extends: _base
cache:
  - { path: "~/.cache/go-build", key: "go-build-ci-override" }
  - { path: "node_modules",      key: "pnpm" }
steps: [{ name: x, run: "true" }]
`,
	})
	p, err := LoadPipeline(filepath.Join(dir, "ci.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(p.Cache) != 2 {
		t.Fatalf("expected 2 cache entries, got %+v", p.Cache)
	}
	// ci.yaml override must win on go-build.
	got := map[string]string{}
	for _, c := range p.Cache {
		got[c.Path] = c.Key
	}
	if got["~/.cache/go-build"] != "go-build-ci-override" {
		t.Errorf("override lost: %v", got)
	}
	if got["node_modules"] != "pnpm" {
		t.Errorf("new entry lost: %v", got)
	}
}

func TestLoadPipeline_ExtendsTargetMissing(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"ci.yaml": `version: 1
kind: pipeline
extends: _missing
steps: [{ name: x, run: "true" }]
`,
	})
	_, err := LoadPipeline(filepath.Join(dir, "ci.yaml"))
	if err == nil {
		t.Fatal("expected missing-target error, got nil")
	}
}

func TestExtendsList_ScalarAndSequence(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"_x.yaml": `version: 1
`,
		"scalar.yaml": `version: 1
kind: pipeline
extends: _x
steps: [{ name: a, run: "true" }]
`,
		"seq.yaml": `version: 1
kind: pipeline
extends: [_x]
steps: [{ name: a, run: "true" }]
`,
	})
	for _, f := range []string{"scalar.yaml", "seq.yaml"} {
		if _, err := LoadPipeline(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}
