package spec

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestDockerContextInputs verifies the cache-key input derivation for a
// docker-build context: real source files are listed, the Dockerfile is always
// included, and .git / node_modules / .claude plus .dockerignore matches are
// pruned (the trees that made the pre-build scan take minutes and bloated the
// key with bytes that never ship).
func TestDockerContextInputs(t *testing.T) {
	repo := t.TempDir()
	write := func(rel, data string) {
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Real sources (should be included).
	write("apps/core/main.go", "package main")
	write("apps/core/Dockerfile", "FROM scratch")
	write("lib/util.go", "package util")
	// Trees that must be pruned.
	write(".git/config", "[core]")
	write("node_modules/dep/index.js", "x")
	write(".claude/worktrees/agent-1/node_modules/foo/a.js", "x")
	write("apps/web/node_modules/bar/b.js", "x")
	// .dockerignore-excluded.
	write(".dockerignore", "**/node_modules\n.git\n.claude\n*.secret\n")
	write("creds.secret", "TOKEN")

	got, err := DockerContextInputs(repo, ".", "apps/core/Dockerfile")
	if err != nil {
		t.Fatalf("dockerContextInputs: %v", err)
	}

	mustHave := []string{"apps/core/Dockerfile", "apps/core/main.go", "lib/util.go"}
	for _, want := range mustHave {
		if !slices.Contains(got, want) {
			t.Errorf("missing expected input %q in %v", want, got)
		}
	}
	mustNotHave := []string{
		".git/config",
		"node_modules/dep/index.js",
		".claude/worktrees/agent-1/node_modules/foo/a.js",
		"apps/web/node_modules/bar/b.js", // nested node_modules pruned too
		"creds.secret",                   // .dockerignore *.secret
	}
	for _, bad := range mustNotHave {
		if slices.Contains(got, bad) {
			t.Errorf("input %q should have been pruned but is present in %v", bad, got)
		}
	}
}

// TestDockerContextInputs_SubdirContext: a context that is a subdir (migrate
// case) lists files repo-relative and still prunes node_modules under it.
func TestDockerContextInputs_SubdirContext(t *testing.T) {
	repo := t.TempDir()
	write := func(rel, data string) {
		p := filepath.Join(repo, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte(data), 0o644)
	}
	write("apps/migrations/Dockerfile", "FROM scratch")
	write("apps/migrations/0001_init.sql", "create table x()")
	write("apps/migrations/node_modules/junk/x.js", "x")
	write("apps/core/main.go", "package main") // outside the context — excluded

	got, err := DockerContextInputs(repo, "apps/migrations", "apps/migrations/Dockerfile")
	if err != nil {
		t.Fatalf("dockerContextInputs: %v", err)
	}
	if !slices.Contains(got, "apps/migrations/0001_init.sql") {
		t.Errorf("expected the migration file, got %v", got)
	}
	for _, bad := range []string{"apps/migrations/node_modules/junk/x.js", "apps/core/main.go"} {
		if slices.Contains(got, bad) {
			t.Errorf("%q should not be a context input, got %v", bad, got)
		}
	}
}
