package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureRepoCloned_SkipsExistingCheckout(t *testing.T) {
	dir := t.TempDir()
	// Fake an existing checkout: just create the .git directory.
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// repoURL is bogus — we want to confirm the function does NOT
	// invoke git when a checkout is already present.
	err := ensureRepoCloned(dir, "https://x-access-token:bad@example.invalid/foo.git", "main", "")
	if err != nil {
		t.Fatalf("expected to skip clone, got error: %v", err)
	}
}

func TestEnsureRepoCloned_RefusesNonEmptyNonGitDir(t *testing.T) {
	dir := t.TempDir()
	// Drop a stray file so the dir is non-empty but not a git checkout.
	if err := os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := ensureRepoCloned(dir, "https://example.invalid/foo.git", "main", "")
	if err == nil {
		t.Fatal("expected refusal, got nil")
	}
	if !strings.Contains(err.Error(), "not empty") {
		t.Errorf("error should mention non-empty dir; got %v", err)
	}
}

func TestRedactSecret_MasksTokenInURL(t *testing.T) {
	in := "fatal: unable to access 'https://x-access-token:ghp_secret123@github.com/foo.git/': 403"
	got := redactSecret(in, "https://x-access-token:ghp_secret123@github.com/foo.git")
	if strings.Contains(got, "ghp_secret123") {
		t.Errorf("token leaked: %q", got)
	}
	if !strings.Contains(got, "https://x-access-token:***@github.com/foo.git") {
		t.Errorf("expected redacted form; got %q", got)
	}
}

func TestRedactSecret_NoMatchIsPassthrough(t *testing.T) {
	in := "fatal: not a git repository"
	got := redactSecret(in, "https://x-access-token:x@example.com")
	if got != in {
		t.Errorf("non-matching input mutated: in=%q out=%q", in, got)
	}
}

func TestSelfCloneRefusesOptionLikeArguments(t *testing.T) {
	for _, args := range [][3]string{
		{"--upload-pack=touch /tmp/x", "", ""},
		{"https://example.com/r.git", "--upload-pack=x", ""},
		{"https://example.com/r.git", "main", "-c"},
	} {
		if err := SelfClone(t.TempDir(), args[0], args[1], args[2]); err == nil {
			t.Errorf("%q accepted", args)
		}
	}
}
