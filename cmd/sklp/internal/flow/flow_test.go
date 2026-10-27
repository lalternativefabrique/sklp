package flow

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepo spins up a throwaway git repo with an initial commit on `main` and
// returns its path. All git calls run from the repo dir (t.Chdir).
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "t@t.io")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "init")
	return dir
}

func TestStartBranchOnly(t *testing.T) {
	gitRepo(t)
	res, err := Start(StartOptions{Name: "login", BranchOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Branch != "feat/login" {
		t.Fatalf("branch = %q, want feat/login", res.Branch)
	}
	if res.Worktree != "" {
		t.Fatalf("worktree = %q, want empty in --branch mode", res.Worktree)
	}
	cur, _ := git("rev-parse", "--abbrev-ref", "HEAD")
	if got := strings.TrimSpace(cur); got != "feat/login" {
		t.Fatalf("HEAD = %q, want feat/login", got)
	}
}

func TestStartRequiresName(t *testing.T) {
	gitRepo(t)
	if _, err := Start(StartOptions{Name: "  "}); err == nil {
		t.Fatal("expected error for empty name")
	}
}

func TestEndDiffsAgainstMain(t *testing.T) {
	gitRepo(t)
	if _, err := Start(StartOptions{Name: "x", BranchOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("new.go", []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := git("add", "."); err != nil {
		t.Fatal(err)
	}
	if _, err := git("commit", "-m", "add new"); err != nil {
		t.Fatal(err)
	}

	res, err := End(EndOptions{}) // default onto = main
	if err != nil {
		t.Fatal(err)
	}
	if res.Onto != "main" {
		t.Fatalf("onto = %q, want main", res.Onto)
	}
	if res.Branch != "feat/x" {
		t.Fatalf("branch = %q, want feat/x", res.Branch)
	}
	if len(res.Changed) != 1 || res.Changed[0] != "new.go" {
		t.Fatalf("changed = %v, want [new.go]", res.Changed)
	}
}

func TestStartWorktree(t *testing.T) {
	dir := gitRepo(t)
	res, err := Start(StartOptions{Name: "wt"})
	if err != nil {
		t.Fatal(err)
	}
	wantRoot := filepath.Join(filepath.Dir(dir), filepath.Base(dir)+"-worktrees")
	wantDir := filepath.Join(wantRoot, "feat-wt")
	if res.Worktree != wantDir {
		t.Fatalf("worktree = %q, want %q", res.Worktree, wantDir)
	}
	if fi, err := os.Stat(res.Worktree); err != nil || !fi.IsDir() {
		t.Fatalf("worktree dir not created: %v", err)
	}
	// Clean up the sibling worktree (outside t.TempDir, so not auto-removed).
	t.Cleanup(func() { _ = os.RemoveAll(wantRoot) })
}

func TestParseSlug(t *testing.T) {
	for _, tc := range []struct{ url, want string }{
		{"git@github.com:digstack/skalpai.git", "digstack/skalpai"},
		{"https://github.com/digstack/skalpai.git", "digstack/skalpai"},
		{"https://github.com/digstack/skalpai", "digstack/skalpai"},
		{"  git@github.com:o/r.git\n", "o/r"},
	} {
		got, err := parseSlug(tc.url)
		if err != nil {
			t.Fatalf("%s: %v", tc.url, err)
		}
		if got != tc.want {
			t.Fatalf("parseSlug(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
	if _, err := parseSlug("https://gitlab.com/o/r"); err == nil {
		t.Fatal("expected error for non-github url")
	}
}

func TestJSONField(t *testing.T) {
	body := `{"id":42,"html_url":"https://github.com/o/r/pull/7","state":"open"}`
	if got := jsonField(body, "html_url"); got != "https://github.com/o/r/pull/7" {
		t.Fatalf("jsonField = %q", got)
	}
	if got := jsonField(body, "missing"); got != "" {
		t.Fatalf("missing key should be empty, got %q", got)
	}
}

func TestFirstPRURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"empty array", "[]", ""},
		{"empty array padded", "  [] \n", ""},
		{"empty body", "", ""},
		{
			"one open pr",
			`[{"number":828,"html_url":"https://github.com/o/r/pull/828","state":"open"}]`,
			"https://github.com/o/r/pull/828",
		},
		{
			"picks the first pr",
			`[{"html_url":"https://github.com/o/r/pull/1"},{"html_url":"https://github.com/o/r/pull/2"}]`,
			"https://github.com/o/r/pull/1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstPRURL(tc.body); got != tc.want {
				t.Fatalf("firstPRURL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEndUnknownTarget(t *testing.T) {
	gitRepo(t)
	if _, err := End(EndOptions{Onto: "nope"}); err == nil {
		t.Fatal("expected error for unknown target ref")
	}
}
