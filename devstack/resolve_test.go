package devstack

import (
	"os"
	"path/filepath"
	"testing"
)

func repoWithDevStacks(t *testing.T, stacks ...string) string {
	t.Helper()
	repo := t.TempDir()
	for _, s := range stacks {
		dir := filepath.Join(repo, ".sklp", s)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "dev.yaml"), []byte("services: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

func TestStackForSpaceResolvesTheStackThatProducedIt(t *testing.T) {
	repo := repoWithDevStacks(t, "core", "web")
	got, ok := StackForSpace(repo, devSpaceName(repo, "web"))
	if !ok {
		t.Fatal("the space's own stack must resolve")
	}
	if got != "web" {
		t.Fatalf("stack = %q, want web", got)
	}
}

func TestStackForSpaceResolvesTheLegacySingleStack(t *testing.T) {
	repo := repoWithDevStacks(t, "core")
	got, ok := StackForSpace(repo, devSpaceName(repo, ""))
	if !ok || got != "" {
		t.Fatalf("StackForSpace = (%q, %v), want the empty legacy stack", got, ok)
	}
}

func TestStackForSpaceRejectsAForeignSpace(t *testing.T) {
	repo := repoWithDevStacks(t, "core")
	if _, ok := StackForSpace(repo, "sklp-dev-deadbeefcafe"); ok {
		t.Fatal("a space no stack of this repo hashes to must not resolve")
	}
}

func TestStackForSpaceIsWorktreeScoped(t *testing.T) {
	a := repoWithDevStacks(t, "core")
	b := repoWithDevStacks(t, "core")
	if _, ok := StackForSpace(b, devSpaceName(a, "core")); ok {
		t.Fatal("another worktree's space must not resolve here")
	}
}
