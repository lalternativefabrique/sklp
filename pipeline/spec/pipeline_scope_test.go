package spec

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestScopeMatches_NoScope(t *testing.T) {
	got, err := ScopeMatches(nil, nil, []string{"apps/web/foo.tsx"})
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("a step with no scope must always run")
	}
}

func TestScopeMatches_EmptyChanged(t *testing.T) {
	// Empty changed list = git failed or nothing changed. We choose to
	// run rather than silently skip. Asserted here so future refactors
	// don't flip the default.
	got, err := ScopeMatches(
		[]string{"backend"},
		map[string][]string{"backend": {"apps/core/**"}},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("empty changed list must default to running scoped steps")
	}
}

func TestScopeMatches_HitMiss(t *testing.T) {
	defs := map[string][]string{
		"backend":  {"apps/core/**", "packages/sdk-go/**"},
		"frontend": {"apps/web/**"},
	}

	cases := []struct {
		name       string
		stepScopes []string
		changed    []string
		want       bool
	}{
		{"backend hit on apps/core", []string{"backend"}, []string{"apps/core/api/handler.go"}, true},
		{"backend hit on sdk-go", []string{"backend"}, []string{"packages/sdk-go/client.go"}, true},
		{"backend miss on web change", []string{"backend"}, []string{"apps/web/App.tsx"}, false},
		{"frontend hit", []string{"frontend"}, []string{"apps/web/App.tsx"}, true},
		{"OR: hit via second scope", []string{"backend", "frontend"}, []string{"apps/web/App.tsx"}, true},
		{"OR: miss on both", []string{"backend", "frontend"}, []string{"docs/index.md"}, false},
		{"mixed changes, hit on one", []string{"backend"}, []string{"docs/x.md", "apps/core/x.go"}, true},
	}
	for _, c := range cases {
		got, err := ScopeMatches(c.stepScopes, defs, c.changed)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestScopeMatches_UnknownScopeErrors(t *testing.T) {
	_, err := ScopeMatches(
		[]string{"typo"},
		map[string][]string{"backend": {"apps/core/**"}},
		[]string{"apps/core/x.go"},
	)
	if err == nil {
		t.Fatal("expected error for undeclared scope, got nil")
	}
	if !strings.Contains(err.Error(), "typo") {
		t.Errorf("error should name the missing scope: %v", err)
	}
}

func TestScopeMatches_BadGlobErrors(t *testing.T) {
	_, err := ScopeMatches(
		[]string{"x"},
		map[string][]string{"x": {"[invalid"}}, // unmatched bracket
		[]string{"a.go"},
	)
	if err == nil {
		t.Fatal("expected error for malformed glob, got nil")
	}
}

// gitInit is a tiny helper: makes dir a git repo with one commit, then
// returns the dir so the test can record a base ref. Skips the test if
// git is not on PATH (the dev environment matters here).
func gitInit(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH, skipping diff-based test")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(cmd.Env,
			"HOME="+dir, // keep config local; avoid touching the user's git config
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "--initial-branch=main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	run("commit", "--allow-empty", "-m", "init")
	return dir
}

func TestChangedFiles_DetectsModifications(t *testing.T) {
	dir := gitInit(t)
	mkfile := func(rel, content string) {
		t.Helper()
		full := filepath.Join(dir, rel)
		if err := exec.Command("mkdir", "-p", filepath.Dir(full)).Run(); err != nil {
			t.Fatal(err)
		}
		if err := exec.Command("sh", "-c", "echo "+content+" > "+full).Run(); err != nil {
			t.Fatal(err)
		}
	}
	gitCmd := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(cmd.Env, "HOME="+dir,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	mkfile("apps/core/x.go", "x")
	mkfile("apps/web/y.tsx", "y")
	gitCmd("add", "-A")
	gitCmd("commit", "-m", "feat: two files")

	got := ChangedFiles(context.Background(), dir, "HEAD~1")
	have := map[string]bool{}
	for _, f := range got {
		have[f] = true
	}
	if !have["apps/core/x.go"] || !have["apps/web/y.tsx"] {
		t.Errorf("expected both files, got %v", got)
	}
}

func TestChangedFiles_GitFailureDegradesOpen(t *testing.T) {
	// Point at a directory that's not a git repo. The function must
	// return nil (not error), so the executor defaults to "run".
	dir := t.TempDir()
	got := ChangedFiles(context.Background(), dir, "")
	if got != nil {
		t.Errorf("expected nil on git failure, got %v", got)
	}
}
