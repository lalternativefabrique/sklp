package main

import (
	"os"
	"path/filepath"
	"testing"
)

// resolveDeployFile must agree with the `deploy` up flow: a <stack> positional
// resolves to .sklp/<stack>/deploy.yaml (not the flat .sklp/deploy.yaml), an
// explicit --file overrides everything, and a bare call falls back to the flat
// legacy default.
func TestResolveDeployFile(t *testing.T) {
	repo := t.TempDir()
	stackDir := filepath.Join(repo, ".sklp", "stack")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stackFile := filepath.Join(stackDir, "deploy.yaml")
	if err := os.WriteFile(stackFile, []byte("version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("stack positional resolves via layout", func(t *testing.T) {
		got, err := resolveDeployFile("stack", "", repo)
		if err != nil {
			t.Fatalf("resolveDeployFile: %v", err)
		}
		if got != stackFile {
			t.Fatalf("got %q, want %q", got, stackFile)
		}
	})

	t.Run("explicit --file wins over stack", func(t *testing.T) {
		got, err := resolveDeployFile("stack", "custom/d.yaml", repo)
		if err != nil {
			t.Fatalf("resolveDeployFile: %v", err)
		}
		if want := filepath.Join(repo, "custom/d.yaml"); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("absolute --file passed through", func(t *testing.T) {
		abs := filepath.Join(repo, "abs.yaml")
		got, err := resolveDeployFile("stack", abs, repo)
		if err != nil {
			t.Fatalf("resolveDeployFile: %v", err)
		}
		if got != abs {
			t.Fatalf("got %q, want %q", got, abs)
		}
	})

	t.Run("no stack and no file falls back to flat default", func(t *testing.T) {
		got, err := resolveDeployFile("", "", repo)
		if err != nil {
			t.Fatalf("resolveDeployFile: %v", err)
		}
		if want := filepath.Join(repo, ".sklp", "deploy.yaml"); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("unknown stack returns an error", func(t *testing.T) {
		if _, err := resolveDeployFile("nope", "", repo); err == nil {
			t.Fatal("expected error for unknown stack, got nil")
		}
	})
}
