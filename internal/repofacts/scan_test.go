package repofacts

import (
	"os"
	"path/filepath"
	"testing"
)

// mkRepo materialises a map of relative-path → content into a fresh
// temp directory and returns the absolute path. The shared helper for
// every fixture-based test in this package.
func mkRepo(t *testing.T, files map[string]string) string {
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

func TestScan_EmptyRepo(t *testing.T) {
	dir := mkRepo(t, nil)
	f, err := Scan(dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if f.Root != dir {
		t.Errorf("root = %q, want %q", f.Root, dir)
	}
	if len(f.Languages) != 0 {
		t.Errorf("expected no languages, got %+v", f.Languages)
	}
	if f.Makefile != nil {
		t.Errorf("expected no makefile, got %+v", f.Makefile)
	}
	if f.Registry != "" {
		t.Errorf("expected no registry, got %q", f.Registry)
	}
}

func TestScan_NotADirectory(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "x.txt")
	_ = os.WriteFile(p, []byte("hi"), 0o644)
	if _, err := Scan(p); err == nil {
		t.Fatal("expected error scanning a file path")
	}
}

func TestScan_GoMonorepo_FindsAllModules(t *testing.T) {
	// The bakery-style layout that the LLM missed: two go.mod
	// under apps/, with a go.work at the root pointing at them.
	dir := mkRepo(t, map[string]string{
		"go.work":            "go 1.25.0\n\nuse (\n\t./apps/core\n\t./apps/sdk-go\n)\n",
		"apps/core/go.mod":   "module github.com/acme/bakery/core\n\ngo 1.25.0\n",
		"apps/core/go.sum":   "",
		"apps/core/main.go":  "package main\n",
		"apps/sdk-go/go.mod": "module github.com/acme/bakery/sdk\n\ngo 1.24.0\n",
		"apps/sdk-go/sdk.go": "package sdk\n",
	})
	f, err := Scan(dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	var golang *LanguageFacts
	for i := range f.Languages {
		if f.Languages[i].Lang == "go" {
			golang = &f.Languages[i]
		}
	}
	if golang == nil {
		t.Fatalf("expected go language, got %+v", f.Languages)
	}
	if len(golang.Modules) != 2 {
		t.Errorf("expected 2 modules (the bug we're fixing), got %d: %+v", len(golang.Modules), golang.Modules)
	}
	if !golang.HasLockfile {
		t.Errorf("go.sum should yield HasLockfile=true")
	}
	if golang.ToolchainVersion != "1.25.0" {
		t.Errorf("toolchain version = %q, want 1.25.0", golang.ToolchainVersion)
	}
	// Module names should be the canonical import paths.
	gotNames := map[string]bool{}
	for _, m := range golang.Modules {
		gotNames[m.Name] = true
	}
	for _, want := range []string{"github.com/acme/bakery/core", "github.com/acme/bakery/sdk"} {
		if !gotNames[want] {
			t.Errorf("missing module name %q in %v", want, gotNames)
		}
	}
}

func TestScan_NodePnpmMonorepo(t *testing.T) {
	dir := mkRepo(t, map[string]string{
		"package.json":             `{"name":"root","workspaces":["apps/*","packages/*"],"engines":{"node":"20.11.0"}}`,
		"pnpm-lock.yaml":           "lockfileVersion: '9.0'\n",
		"pnpm-workspace.yaml":      "packages:\n  - apps/*\n  - packages/*\n",
		"apps/web/package.json":    `{"name":"@acme/web"}`,
		"packages/ui/package.json": `{"name":"@acme/ui"}`,
	})
	f, err := Scan(dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	var node *LanguageFacts
	for i := range f.Languages {
		if f.Languages[i].Lang == "node" {
			node = &f.Languages[i]
		}
	}
	if node == nil {
		t.Fatalf("expected node language, got %+v", f.Languages)
	}
	if node.PackageManager != "pnpm" {
		t.Errorf("package_manager = %q, want pnpm", node.PackageManager)
	}
	if node.ToolchainVersion != "20.11.0" {
		t.Errorf("node version = %q, want 20.11.0", node.ToolchainVersion)
	}
	if !node.HasLockfile {
		t.Errorf("pnpm-lock.yaml should yield HasLockfile=true")
	}
	if len(node.Modules) != 3 {
		// root + apps/web + packages/ui = 3
		t.Errorf("expected 3 modules, got %d", len(node.Modules))
	}
}

func TestScan_RustWorkspace(t *testing.T) {
	dir := mkRepo(t, map[string]string{
		"Cargo.toml":            "[workspace]\nmembers = [\"crates/*\"]\n",
		"Cargo.lock":            "",
		"rust-toolchain.toml":   `[toolchain]` + "\n" + `channel = "1.78.0"` + "\n",
		"crates/cli/Cargo.toml": `[package]` + "\n" + `name = "acme-cli"` + "\n",
	})
	f, err := Scan(dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	var rust *LanguageFacts
	for i := range f.Languages {
		if f.Languages[i].Lang == "rust" {
			rust = &f.Languages[i]
		}
	}
	if rust == nil {
		t.Fatalf("expected rust language")
	}
	if rust.ToolchainVersion != "1.78.0" {
		t.Errorf("toolchain = %q, want 1.78.0", rust.ToolchainVersion)
	}
	if !rust.HasLockfile {
		t.Errorf("Cargo.lock should set HasLockfile")
	}
	// Workspace + member crate = 2 modules.
	if len(rust.Modules) != 2 {
		t.Errorf("expected 2 cargo modules, got %d", len(rust.Modules))
	}
}

func TestScan_PythonUvProject(t *testing.T) {
	dir := mkRepo(t, map[string]string{
		"pyproject.toml":  "[project]\nname = \"acme\"\nrequires-python = \">=3.11\"\n",
		"uv.lock":         "",
		".python-version": "3.12.0\n",
	})
	f, err := Scan(dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	var py *LanguageFacts
	for i := range f.Languages {
		if f.Languages[i].Lang == "python" {
			py = &f.Languages[i]
		}
	}
	if py == nil {
		t.Fatalf("expected python language")
	}
	if py.PackageManager != "uv" {
		t.Errorf("package_manager = %q, want uv", py.PackageManager)
	}
	if py.ToolchainVersion != "3.12.0" {
		t.Errorf("toolchain = %q, want 3.12.0", py.ToolchainVersion)
	}
}

func TestScan_DockerfilesAndCompose(t *testing.T) {
	dir := mkRepo(t, map[string]string{
		"Dockerfile":          "FROM alpine\n",
		"Dockerfile.worker":   "FROM alpine\n",
		"apps/web/Dockerfile": "FROM node\n",
		"compose.yml":         "services:\n  api:\n    image: ghcr.io/acme/api:latest\n",
	})
	f, err := Scan(dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(f.Dockerfiles) != 3 {
		t.Errorf("expected 3 dockerfiles, got %v", f.Dockerfiles)
	}
	if len(f.ComposeFiles) != 1 {
		t.Errorf("expected 1 compose file, got %v", f.ComposeFiles)
	}
}

func TestScan_MakefileTargets(t *testing.T) {
	dir := mkRepo(t, map[string]string{
		"Makefile": `.PHONY: test build docker-build-core

REGISTRY ?= rg.fr-par.scw.cloud/acme

test:
	go test ./...

build:
	go build

docker-build-core:
	docker build -t $(REGISTRY)/core:$(IMAGE_TAG) .

# This is a pattern rule — should be skipped.
%.o: %.c
	cc -c $<
`,
	})
	f, err := Scan(dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if f.Makefile == nil {
		t.Fatal("expected makefile")
	}
	want := map[string]bool{"test": true, "build": true, "docker-build-core": true}
	got := map[string]bool{}
	for _, t := range f.Makefile.Targets {
		got[t] = true
	}
	for k := range want {
		if !got[k] {
			t.Errorf("missing target %q in %v", k, f.Makefile.Targets)
		}
	}
	// Pattern rule should not leak in.
	if got["%.o"] {
		t.Errorf("pattern rule leaked into targets: %v", f.Makefile.Targets)
	}
	if f.Registry != "rg.fr-par.scw.cloud/acme" {
		t.Errorf("registry = %q, want rg.fr-par.scw.cloud/acme", f.Registry)
	}
}

func TestScan_CIHints(t *testing.T) {
	dir := mkRepo(t, map[string]string{
		".github/workflows/ci.yml":      "name: CI\n",
		".github/workflows/deploy.yaml": "name: deploy\n",
		".gitlab-ci.yml":                "stages: [test]\n",
		".drone.yml":                    "kind: pipeline\n",
	})
	f, err := Scan(dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(f.CIHints.GitHubActions) != 2 {
		t.Errorf("github actions hint count wrong: %v", f.CIHints.GitHubActions)
	}
	if len(f.CIHints.GitLabCI) != 1 || len(f.CIHints.Drone) != 1 {
		t.Errorf("ci hints wrong: %+v", f.CIHints)
	}
}

func TestScan_IgnoresNodeModulesAndVendor(t *testing.T) {
	dir := mkRepo(t, map[string]string{
		"package.json":                   `{"name":"root"}`,
		"pnpm-lock.yaml":                 "",
		"node_modules/foo/package.json":  `{"name":"foo"}`,
		"node_modules/foo/index.js":      "",
		"vendor/github.com/lib/x/go.mod": "module x\n",
	})
	f, err := Scan(dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	// Should detect node from the root, but NOT include node_modules
	// or vendor packages as modules.
	for _, l := range f.Languages {
		for _, m := range l.Modules {
			if m.Path != "" && (containsSegment(m.Path, "node_modules") || containsSegment(m.Path, "vendor")) {
				t.Errorf("module from ignored dir leaked: %+v", m)
			}
		}
	}
}

// containsSegment is a tiny helper duplicated from util.go to keep
// the test self-contained against future refactors.
func containsSegment(path, segment string) bool {
	for _, p := range splitSlash(path) {
		if p == segment {
			return true
		}
	}
	return false
}

func splitSlash(s string) []string {
	out := []string{}
	cur := ""
	for _, c := range s {
		if c == '/' || c == filepath.Separator {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(c)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
