package stackconfig

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// withConfig points the sklp config at a temp file holding cmdline as the
// vault lookup, so a test never reads or writes the developer's own config.
func withConfig(t *testing.T, cmdline string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "api: {}\n"
	if cmdline != "" {
		body += "secrets:\n  passphrase_command: " + cmdline + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SKLP_CONFIG", path)
}

func TestResolvePassphraseEnvWins(t *testing.T) {
	withConfig(t, "/bin/echo from-command")
	t.Setenv(PassphraseEnv, "from-env")

	v, src, err := ResolvePassphrase(".")
	if err != nil {
		t.Fatalf("ResolvePassphrase: %v", err)
	}
	if v != "from-env" || src != SourceEnv {
		t.Fatalf("got (%q, %s), want (from-env, env) — an exported value must outrank the vault", v, src)
	}
}

func TestResolvePassphraseFallsBackToCommand(t *testing.T) {
	withConfig(t, "/bin/echo from-command")
	t.Setenv(PassphraseEnv, "")

	v, src, err := ResolvePassphrase(".")
	if err != nil {
		t.Fatalf("ResolvePassphrase: %v", err)
	}
	if v != "from-command" || src != SourceCommand {
		t.Fatalf("got (%q, %s), want (from-command, command)", v, src)
	}
}

// TestResolvePassphraseTrimsTrailingNewline: echo appends \n, as does every
// vault CLI. PBKDF2 folds it into the key, so an untrimmed value derives a
// different key and fails later as an indistinguishable "wrong passphrase".
func TestResolvePassphraseTrimsTrailingNewline(t *testing.T) {
	withConfig(t, "/bin/echo secret")
	t.Setenv(PassphraseEnv, "")

	v, _, err := ResolvePassphrase(".")
	if err != nil {
		t.Fatalf("ResolvePassphrase: %v", err)
	}
	if v != "secret" {
		t.Fatalf("got %q, want %q with no trailing newline", v, "secret")
	}
}

// A command that cannot run at all is a misconfiguration the operator must
// see: the binary is missing or not executable, and every later run would
// fail as an opaque "wrong passphrase".
func TestResolvePassphraseUnrunnableCommandIsAnError(t *testing.T) {
	withConfig(t, "/nonexistent/vault-cli")
	t.Setenv(PassphraseEnv, "")

	if _, _, err := ResolvePassphrase("."); err == nil {
		t.Fatal("an unrunnable lookup must be reported, not silently treated as absent")
	}
}

// A command that runs and finds nothing is not an error. Every vault CLI
// exits non-zero on a missing entry, and the lookup is now the default path
// for every repo — including one that declares no secret and must still run
// on a machine whose vault holds no key for it.
func TestResolvePassphraseEmptyVaultIsNotAnError(t *testing.T) {
	withConfig(t, "/bin/false")
	t.Setenv(PassphraseEnv, "")

	v, src, err := ResolvePassphrase(".")
	if err != nil {
		t.Fatalf("a vault with no entry must not be an error, got %v", err)
	}
	if v != "" || src != SourceNone {
		t.Fatalf("got (%q, %s), want (\"\", none)", v, src)
	}
}

// TestResolvePassphraseEmptyIsNotAnError: a pipeline declaring no secrets
// never derives a key, so it must run on a machine with no vault and no
// export. Turning "absent" into an error would demand the deploy vault's key
// from a lint run.
func TestResolvePassphraseEmptyIsNotAnError(t *testing.T) {
	withConfig(t, "/bin/echo")
	t.Setenv(PassphraseEnv, "")

	v, src, err := ResolvePassphrase(".")
	if err != nil {
		t.Fatalf("an absent passphrase must not be an error, got %v", err)
	}
	if v != "" || src != SourceNone {
		t.Fatalf("got (%q, %s), want (\"\", none)", v, src)
	}
}

// TestRepoKeyUsesMainWorktree: a worktree shares the main checkout's config
// files, so it must resolve to the same vault entry. --show-toplevel would
// return the worktree dir and give every feature branch its own entry.
func TestRepoKeyUsesMainWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	main := filepath.Join(root, "myrepo")
	if err := os.MkdirAll(main, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(main, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(main, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(main, "add", "f")
	run(main, "commit", "-qm", "init")

	if got := RepoKey(main); got != "myrepo" {
		t.Fatalf("RepoKey(main) = %q, want myrepo", got)
	}

	wt := filepath.Join(root, "feat-something")
	run(main, "worktree", "add", "-q", "-b", "feat/x", wt)
	if got := RepoKey(wt); got != "myrepo" {
		t.Fatalf("RepoKey(worktree) = %q, want myrepo — a worktree shares the main repo's vault entry", got)
	}
}

// TestRunVaultCommandIgnoresEnv: the validation done by `sklp config
// passphrase command` must run the command itself, not go through
// ResolvePassphrase — there the env var wins, so an operator who still
// exports the passphrase would be told a working command "produced no
// passphrase".
func TestRunVaultCommandIgnoresEnv(t *testing.T) {
	t.Setenv(PassphraseEnv, "from-env")

	v, err := RunVaultCommand("/bin/echo from-command", ".")
	if err != nil {
		t.Fatalf("RunVaultCommand: %v", err)
	}
	if v != "from-command" {
		t.Fatalf("got %q, want from-command — the env var must not shadow the command being validated", v)
	}
}

// TestRunVaultCommandSubstitutesRepo: one machine-wide passphrase_command must
// serve every repo. Without the placeholder the repo name has to be hard-coded
// in the command, so a second repo sealed under its own passphrase silently
// gets the first one's vault entry.
func TestRunVaultCommandSubstitutesRepo(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "myrepo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	v, err := RunVaultCommand("/bin/echo account="+RepoPlaceholder, dir)
	if err != nil {
		t.Fatalf("RunVaultCommand: %v", err)
	}
	if v != "account=myrepo" {
		t.Fatalf("got %q, want account=myrepo", v)
	}
}

// TestRunVaultCommandWithoutPlaceholderIsUnchanged: every command configured
// before the placeholder existed must keep running verbatim.
func TestRunVaultCommandWithoutPlaceholderIsUnchanged(t *testing.T) {
	v, err := RunVaultCommand("/bin/echo plain", t.TempDir())
	if err != nil {
		t.Fatalf("RunVaultCommand: %v", err)
	}
	if v != "plain" {
		t.Fatalf("got %q, want plain", v)
	}
}
