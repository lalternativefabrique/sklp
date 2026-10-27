package stackconfig

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/lalternativefabrique/sklp/internal/config"
)

// Source says where a resolved passphrase came from, for `sklp config
// passphrase status` and for error messages that would otherwise blame the
// env var for a value the vault supplied.
type Source string

const (
	SourceEnv      Source = "env"
	SourceCommand  Source = "command"
	SourceKeychain Source = "keychain"
	SourceNone     Source = "none"
)

// KeychainService is the service name for the default OS-vault lookup, `-s`
// for macOS `security` and the `service` attribute for libsecret. The account
// is the repo name, so two repos sealed under different passphrases
// (different salts) do not collide on one entry.
const KeychainService = "sklp"

// ErrVaultEmpty reports a lookup that ran and found no entry, as opposed to
// one that could not run. No exit code separates the two — every vault CLI
// exits non-zero on a missing entry — so the distinction is made here.
var ErrVaultEmpty = errors.New("vault holds no passphrase for this repo")

// RepoPlaceholder is replaced by RepoKey in a configured passphrase_command.
// Braces, not $VAR: there is no shell to expand a variable, and the literal
// spells out that sklp substitutes it, so the operator reads what will run
// from the config file itself.
const RepoPlaceholder = "{repo}"

// ResolvePassphrase returns the passphrase for repoRoot and where it came
// from, trying in order: the env var, a configured command, then the OS
// vault.
//
// The env var wins so a CI run, a relayed VM and `env VAR=… sklp` keep
// behaving exactly as before; the vault is the fallback that spares an
// operator from exporting anything.
//
// An empty result is NOT an error. ResolveWanted only derives a key when a
// secret is actually requested, so a pipeline declaring no secrets must still
// run on a machine with no vault and no export — asking it for the deploy
// vault's key would be the regression.
func ResolvePassphrase(repoRoot string) (string, Source, error) {
	if v := os.Getenv(PassphraseEnv); v != "" {
		return v, SourceEnv, nil
	}
	cfg, _, err := config.Load()
	if err != nil {
		return "", SourceNone, fmt.Errorf("load sklp config: %w", err)
	}
	if cmd := strings.TrimSpace(cfg.Secrets.PassphraseCommand); cmd != "" {
		v, err := RunVaultCommand(cmd, repoRoot)
		switch {
		case err == nil && v != "":
			return v, SourceCommand, nil
		case errors.Is(err, ErrVaultEmpty):
			// It ran and found nothing. Every vault CLI exits non-zero on a
			// missing entry, and the lookup is the default path for every
			// repo — including one that declares no secret and must still
			// run on a machine whose vault holds no key for it. Turning
			// that into a fatal error would demand the deploy vault's key
			// from a lint run.
			return "", SourceNone, nil
		default:
			// It could not run at all: binary missing, not executable, no
			// permission. That is a misconfiguration the operator must see,
			// not a vault that happens to be empty.
			return "", SourceNone, err
		}
	}
	v, err := VaultLookup(RepoKey(repoRoot))
	if err != nil || v == "" {
		return "", SourceNone, nil
	}
	return v, SourceKeychain, nil
}

// VaultLookup reads the repo's entry from the OS vault: `security` on
// macOS, libsecret on Linux. Hard-coding these two is the one exception to
// "sklp implements no vault", and it is what makes the default per-repo
// without any configuration; anything else is reached through
// passphrase_command. A missing helper is not an error — it is a machine
// with no vault, which ResolvePassphrase reports as SourceNone.
func VaultLookup(account string) (string, error) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("security", "find-generic-password",
			"-a", account, "-s", KeychainService, "-w")
	case "linux":
		cmd = exec.Command("secret-tool", "lookup",
			"service", KeychainService, "account", account)
	default:
		return "", errors.New("no OS vault on " + runtime.GOOS)
	}
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\r\n \t"), nil
}

// RunVaultCommand executes the configured lookup for repoRoot and returns its
// stdout.
//
// No shell: the command is split on whitespace and exec'd directly, so a
// value never reaches a shell that could expand or log it, and a config file
// cannot smuggle `; rm -rf` past the operator who wrote it. RepoPlaceholder is
// therefore substituted here, after the split, and never by a shell: one
// machine-wide config then serves every repo, each with its own vault entry,
// the way the macOS default already keys on RepoKey. Substituting after the
// split keeps a key holding a space from becoming two arguments.
//
// Trailing whitespace is stripped because every vault CLI prints a newline
// and PBKDF2 folds it into the key: keeping it derives a different key and
// fails as an indistinguishable "wrong passphrase" much later, inside a run.
func RunVaultCommand(cmdline, repoRoot string) (string, error) {
	fields := strings.Fields(cmdline)
	if len(fields) == 0 {
		return "", errors.New("passphrase_command is empty")
	}
	if strings.Contains(cmdline, RepoPlaceholder) {
		key := RepoKey(repoRoot)
		for i, f := range fields {
			fields[i] = strings.ReplaceAll(f, RepoPlaceholder, key)
		}
	}
	out, err := exec.Command(fields[0], fields[1:]...).Output()
	if err != nil {
		// Never wrap an ExitError: it carries the command's stderr, and a
		// vault CLI may echo the secret back in a diagnostic. Name the
		// binary, not what it printed. ErrVaultEmpty lets the caller tell
		// "it ran and found nothing" from "it could not run at all", which
		// no exit code distinguishes.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("%w: passphrase_command %q found nothing", ErrVaultEmpty, fields[0])
		}
		// Not an ExitError: the binary is missing, not executable, or was
		// killed. No command output is involved, so the cause is safe to
		// carry and is what the operator needs to fix it.
		return "", fmt.Errorf("passphrase_command %q failed: %w", fields[0], err)
	}
	return strings.TrimRight(string(out), "\r\n \t"), nil
}

// RepoKey is the vault account name for a repo: the directory name of the
// MAIN worktree.
//
// git rev-parse --git-common-dir is what makes worktrees work. --show-toplevel
// would return the worktree's own directory (feat-something), so every feature
// branch would demand its own vault entry for config files it shares verbatim
// with the main checkout.
func RepoKey(repoRoot string) string {
	out, err := exec.Command("git", "-C", repoRoot, "rev-parse", "--git-common-dir").Output()
	if err == nil {
		if dir := strings.TrimSpace(string(out)); dir != "" {
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(repoRoot, dir)
			}
			if abs, err := filepath.Abs(filepath.Dir(dir)); err == nil {
				return filepath.Base(abs)
			}
		}
	}
	if abs, err := filepath.Abs(repoRoot); err == nil {
		return filepath.Base(abs)
	}
	return filepath.Base(repoRoot)
}
