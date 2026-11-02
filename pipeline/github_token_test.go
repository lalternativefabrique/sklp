package pipeline

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestRedactToken(t *testing.T) {
	cases := []struct {
		in   string
		want string // substring that must NOT appear, and one that must
	}{
		{"https://oauth2:ghp_SECRETTOKEN123@github.com/x", "ghp_SECRETTOKEN123"},
		{"url.https://x-access-token:ghs_abc999@github.com/.insteadOf", "ghs_abc999"},
		{"plain github_pat_11ABCDEF_xyz leaked", "github_pat_11ABCDEF_xyz"},
		{"bare gho_deadbeef token", "gho_deadbeef"},
	}
	for _, c := range cases {
		got := redactToken(c.in)
		if strings.Contains(got, c.want) {
			t.Errorf("redactToken(%q) = %q, still contains secret %q", c.in, got, c.want)
		}
		if !strings.Contains(got, "***") {
			t.Errorf("redactToken(%q) = %q, expected a *** marker", c.in, got)
		}
	}
}

func TestRedactToken_NoFalsePositive(t *testing.T) {
	clean := "fatal: not a git repository: /workspace/.git"
	if got := redactToken(clean); got != clean {
		t.Errorf("redactToken mangled clean string: %q", got)
	}
}

func TestStripGitVars(t *testing.T) {
	in := []string{"PATH=/bin", "GIT_DIR=/repo/.git", "HOME=/tmp", "GIT_WORK_TREE=/repo", "GIT_COMMON_DIR=/x"}
	got := stripGitVars(in)
	for _, kv := range got {
		if strings.HasPrefix(kv, "GIT_") {
			t.Errorf("stripGitVars left a GIT_ var: %q", kv)
		}
	}
	if len(got) != 2 {
		t.Errorf("stripGitVars kept %d vars, want 2 (PATH, HOME): %v", len(got), got)
	}
}

// WireGitHubToken must write an insteadOf rewrite for BOTH the HTTPS and SSH
// forms of github.com, so a private module pulled over git@github.com: (a
// go.mod replace / .gitmodules SSH url) is rewritten to the token URL instead
// of cloning anonymously. Running it twice must stay idempotent (it fires once
// early in Run() and again after env_files load).
func TestWireGitHubToken_RewritesHTTPSAndSSH(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	home := t.TempDir()
	cfg := home + "/.gitconfig"
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("HOME", home)
	t.Setenv("GITHUB_TOKEN", "ghp_TESTTOKEN123")
	// The global write is sandbox-only now; off-sandbox the rewrite goes to
	// the environment instead (see TestWireGitHubTokenOffSandbox...).
	t.Setenv(RunnerModeEnv, RunnerModeK8s)

	for pass := 1; pass <= 2; pass++ {
		if err := WireGitHubToken(io.Discard); err != nil {
			t.Fatalf("pass %d: WireGitHubToken: %v", pass, err)
		}
		key := "url.https://oauth2:ghp_TESTTOKEN123@github.com/.insteadOf"
		out, err := exec.Command("git", "config", "--global", "--get-all", key).Output()
		if err != nil {
			t.Fatalf("pass %d: get-all: %v", pass, err)
		}
		got := strings.TrimSpace(string(out))
		for _, want := range []string{"https://github.com/", "ssh://git@github.com/", "git@github.com:"} {
			if !strings.Contains(got, want) {
				t.Errorf("pass %d: rewrite for %q missing; got:\n%s", pass, want, got)
			}
		}
		// Idempotent: exactly 3 lines, no accumulation across passes.
		if n := len(strings.Split(got, "\n")); n != 3 {
			t.Errorf("pass %d: expected 3 rewrites, got %d:\n%s", pass, n, got)
		}
	}
}

// runGit must not abort when the caller sits in a broken worktree whose
// GIT_DIR points nowhere — the global config write is repo-independent.
func TestRunGit_IgnoresBrokenGitEnv(t *testing.T) {
	if _, err := os.Stat("/usr/bin/git"); err != nil {
		if _, err := os.Stat("/bin/git"); err != nil {
			t.Skip("git not available")
		}
	}
	t.Setenv("GIT_DIR", "/nonexistent/path/.git/worktrees/ghost")
	t.Setenv("GIT_WORK_TREE", "/nonexistent/path")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "")
	// A harmless global read; must succeed despite the poisoned GIT_* env.
	if err := runGit("config", "--global", "--add", "safe.directory", "/workspace"); err != nil {
		t.Fatalf("runGit aborted on broken GIT_* env: %v", err)
	}
}

// Off-sandbox the user's own config must stay untouched: the run gets a
// global config of its own, which carries the user's settings minus their url
// rewrites plus the token one, so the token wins even against an https→ssh
// rewrite in ~/.gitconfig.
func TestWireGitHubTokenOffSandboxLeavesUserConfigAlone(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	home := t.TempDir()
	cfg := home + "/.gitconfig"
	user := "[url \"git@github.com:\"]\n\tinsteadOf = https://github.com/\n[user]\n\tname = Someone\n"
	if err := os.WriteFile(cfg, []byte(user), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")
	t.Setenv("GIT_CONFIG_GLOBAL", "")
	os.Unsetenv("GIT_CONFIG_GLOBAL")
	t.Setenv("GITHUB_TOKEN", "ghp_OFFSANDBOX123")
	t.Setenv(RunnerModeEnv, "")

	if err := WireGitHubToken(io.Discard); err != nil {
		t.Fatalf("WireGitHubToken: %v", err)
	}

	if b, _ := os.ReadFile(cfg); string(b) != user {
		t.Fatalf("the user's config changed:\n%s", b)
	}
	run := os.Getenv("GIT_CONFIG_GLOBAL")
	if run == "" || run == cfg {
		t.Fatalf("GIT_CONFIG_GLOBAL = %q, want the run's own file", run)
	}
	for _, from := range []string{"https://github.com/org/repo", "ssh://git@github.com/org/repo", "git@github.com:org/repo"} {
		out, err := exec.Command("git", "ls-remote", "--get-url", from).Output()
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(out)); got != "https://oauth2:ghp_OFFSANDBOX123@github.com/org/repo" {
			t.Fatalf("%s rewritten to %q", from, got)
		}
	}
	name, _ := exec.Command("git", "config", "--global", "user.name").Output()
	if strings.TrimSpace(string(name)) != "Someone" {
		t.Fatalf("user settings lost: user.name = %q", name)
	}
}
