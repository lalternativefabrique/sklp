package runnerimage

import (
	"os"
	"path/filepath"
	"testing"
)

func mkSklp(t *testing.T) (repo, sklp string) {
	t.Helper()
	repo = t.TempDir()
	sklp = filepath.Join(repo, ".sklp")
	if err := os.MkdirAll(sklp, 0o755); err != nil {
		t.Fatal(err)
	}
	return repo, sklp
}

// space.yaml present → returned as-is, no migration, legacy left untouched.
func TestResolvePath_PrefersSpace(t *testing.T) {
	repo, sklp := mkSklp(t)
	space := filepath.Join(sklp, "space.yaml")
	if err := os.WriteFile(space, []byte("runner: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A stale legacy file alongside must be ignored, not migrated over.
	legacy := filepath.Join(sklp, "config.yaml")
	_ = os.WriteFile(legacy, []byte("runner: {legacy: true}\n"), 0o644)

	if got := ResolvePath(repo); got != space {
		t.Fatalf("ResolvePath = %q, want %q", got, space)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Errorf("legacy config.yaml should be left untouched when space.yaml exists: %v", err)
	}
}

// only config.yaml present → auto-migrated to space.yaml (renamed in place),
// content preserved, the new path returned, the old name gone.
func TestResolvePath_AutoMigratesLegacy(t *testing.T) {
	repo, sklp := mkSklp(t)
	legacy := filepath.Join(sklp, "config.yaml")
	body := []byte("runner:\n  registry: example.com/x\n")
	if err := os.WriteFile(legacy, body, 0o644); err != nil {
		t.Fatal(err)
	}

	got := ResolvePath(repo)
	want := filepath.Join(sklp, "space.yaml")
	if got != want {
		t.Fatalf("ResolvePath = %q, want %q", got, want)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("legacy config.yaml should have been renamed away, stat err = %v", err)
	}
	gotBody, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("read migrated file: %v", err)
	}
	if string(gotBody) != string(body) {
		t.Errorf("migrated content = %q, want %q", gotBody, body)
	}
}

// neither present → canonical space.yaml path returned (so callers emit a
// space.yaml-named error, not a confusing legacy one).
func TestResolvePath_NeitherReturnsCanonical(t *testing.T) {
	repo, sklp := mkSklp(t)
	if got := ResolvePath(repo); got != filepath.Join(sklp, "space.yaml") {
		t.Fatalf("ResolvePath = %q, want canonical space.yaml", got)
	}
}

// no flat recipe but exactly one stack carries .sklp/<stack>/space.yaml → that
// stack-scoped path is returned, so a single-stack repo can keep the recipe
// next to its other contracts instead of at the .sklp root.
func TestResolvePath_StackScopedSingle(t *testing.T) {
	repo, sklp := mkSklp(t)
	stackDir := filepath.Join(sklp, "stack")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(stackDir, "space.yaml")
	if err := os.WriteFile(want, []byte("runner: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ResolvePath(repo); got != want {
		t.Fatalf("ResolvePath = %q, want stack-scoped %q", got, want)
	}
}

// a flat .sklp/space.yaml wins over a stack-scoped one (the flat path is the
// canonical repo-global recipe; the stack copy is only a fallback).
func TestResolvePath_FlatWinsOverStack(t *testing.T) {
	repo, sklp := mkSklp(t)
	flat := filepath.Join(sklp, "space.yaml")
	if err := os.WriteFile(flat, []byte("runner: {flat: true}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stackDir := filepath.Join(sklp, "stack")
	_ = os.MkdirAll(stackDir, 0o755)
	_ = os.WriteFile(filepath.Join(stackDir, "space.yaml"), []byte("runner: {stack: true}\n"), 0o644)

	if got := ResolvePath(repo); got != flat {
		t.Fatalf("ResolvePath = %q, want flat %q", got, flat)
	}
}

// two stacks each declare a space.yaml → ambiguous, so NOT stack-resolved; the
// canonical flat path is returned (the recipe is repo-global, it cannot live in
// two stacks at once — the caller must hoist it to the root).
func TestResolvePath_MultipleStacksAmbiguous(t *testing.T) {
	repo, sklp := mkSklp(t)
	for _, s := range []string{"core", "edge"} {
		dir := filepath.Join(sklp, s)
		_ = os.MkdirAll(dir, 0o755)
		_ = os.WriteFile(filepath.Join(dir, "space.yaml"), []byte("runner: {}\n"), 0o644)
	}
	if got := ResolvePath(repo); got != filepath.Join(sklp, "space.yaml") {
		t.Fatalf("ResolvePath = %q, want canonical flat space.yaml on ambiguity", got)
	}
}
