package pipeline

import (
	"fmt"
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

// On a developer machine the rewrite must NOT reach ~/.gitconfig: it would
// outlive the run and redirect every later github operation to a token that
// has since expired — including plain `git push`.
func TestWireGitHubTokenOffSandboxLeavesUserConfigAlone(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	home := t.TempDir()
	cfg := home + "/.gitconfig"
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("HOME", home)
	t.Setenv("GITHUB_TOKEN", "ghp_OFFSANDBOX123")
	t.Setenv(RunnerModeEnv, "")
	t.Setenv("GIT_CONFIG_COUNT", "")

	if err := WireGitHubToken(io.Discard); err != nil {
		t.Fatalf("WireGitHubToken: %v", err)
	}

	if b, err := os.ReadFile(cfg); err == nil && strings.Contains(string(b), "insteadOf") {
		t.Fatalf("wrote an insteadOf rule into the user's config:\n%s", b)
	}
	if got := os.Getenv("GIT_CONFIG_COUNT"); got != "3" {
		t.Fatalf("GIT_CONFIG_COUNT = %q, want 3 (https, ssh://, scp-like)", got)
	}
	found := map[string]bool{}
	for i := 0; i < 3; i++ {
		if k := os.Getenv(fmt.Sprintf("GIT_CONFIG_KEY_%d", i)); !strings.Contains(k, "insteadOf") {
			t.Fatalf("key %d = %q", i, k)
		}
		found[os.Getenv(fmt.Sprintf("GIT_CONFIG_VALUE_%d", i))] = true
	}
	for _, want := range []string{"https://github.com/", "ssh://git@github.com/", "git@github.com:"} {
		if !found[want] {
			t.Fatalf("rewrite for %q missing: %v", want, found)
		}
	}
}

// A caller that already exported git config entries must keep them.
func TestExportGitConfigEnvPreservesExisting(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "safe.directory")
	t.Setenv("GIT_CONFIG_VALUE_0", "/repo")

	if err := exportGitConfigEnv("url.x.insteadOf", []string{"https://github.com/"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if os.Getenv("GIT_CONFIG_COUNT") != "2" {
		t.Fatalf("count = %q, want 2", os.Getenv("GIT_CONFIG_COUNT"))
	}
	if os.Getenv("GIT_CONFIG_KEY_0") != "safe.directory" {
		t.Fatal("clobbered a pre-existing entry")
	}
}
