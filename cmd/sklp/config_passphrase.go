package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"

	"github.com/lalternativefabrique/sklp/internal/config"
	"github.com/lalternativefabrique/sklp/internal/stackconfig"
)

// config_passphrase.go implements `sklp config passphrase {set,status,unset}`.
//
// Only the OS vault is written to (macOS keychain, libsecret on Linux):
// writing is the one operation that cannot be expressed as a generic command,
// since every vault CLI takes its input differently. Reading stays
// vault-agnostic through secrets.passphrase_command — see
// stackconfig.ResolvePassphrase.

func runPassphraseSet() int {
	if !hasOSVault() {
		fmt.Fprintf(os.Stderr, "sklp config passphrase set: no OS vault on this machine.\n"+
			"Store it in your own vault and point sklp at it:\n"+
			"  sklp config passphrase command '<command printing the passphrase>'\n")
		return 2
	}
	account := stackconfig.RepoKey(".")
	fmt.Printf("Storing the passphrase for repo %q in the OS vault.\n", account)

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		// `security -w` with no value prompts on the TERMINAL, not stdin, so
		// it must own the prompting: asking here as well made the user type
		// the passphrase four times and stored whatever security's own prompt
		// read, which is not necessarily what sklp had asked for.
		//
		// -U updates an existing entry; without it security refuses with "The
		// specified item already exists in the keychain".
		cmd = exec.Command("security", "add-generic-password", "-U",
			"-a", account, "-s", stackconfig.KeychainService, "-w")
	default:
		// secret-tool reads the value from stdin and prompts itself when that
		// is a terminal, so it owns the prompting for the same reason.
		cmd = exec.Command("secret-tool", "store", "--label", "sklp "+account,
			"service", stackconfig.KeychainService, "account", account)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "sklp config passphrase set: vault write failed")
		return 1
	}

	// Verify against the repo's own files: a passphrase stored but wrong is
	// exactly the failure this command exists to prevent, and it would
	// otherwise surface much later as an opaque AEAD error inside a run.
	stored, err := stackconfig.VaultLookup(account)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp config passphrase set: stored, but could not read it back")
		return 1
	}
	fmt.Printf("stored the passphrase for %q (service %q)\n", account, stackconfig.KeychainService)
	return reportDecryption(stored)
}

func osVaultName() string {
	if runtime.GOOS == "darwin" {
		return "macOS keychain"
	}
	return "libsecret"
}

func hasOSVault() bool {
	switch runtime.GOOS {
	case "darwin":
		return true
	case "linux":
		_, err := exec.LookPath("secret-tool")
		return err == nil
	}
	return false
}

// runPassphraseStatus reports where the passphrase comes from and whether it
// actually opens this repo's files. The value is never printed, nor a prefix,
// nor its length: a wrong-passphrase failure surfaces deep inside a run as an
// AEAD error, and that opacity is what this command exists to remove — not by
// showing the secret, but by naming the source and the verdict.
func runPassphraseStatus() int {
	account := stackconfig.RepoKey(".")
	fmt.Printf("repo:   %s\n", account)

	// Resolve reloads the config itself; do the lookup first so its own
	// stderr warnings (loose permissions) are emitted once, not twice.
	pp, src, resolveErr := stackconfig.ResolvePassphrase(".")

	if cfg, _, err := config.Load(); err == nil && cfg.Secrets.PassphraseCommand != "" {
		fmt.Printf("vault:  %s\n", cfg.Secrets.PassphraseCommand)
	} else if hasOSVault() {
		fmt.Printf("vault:  %s (service %q, account %q)\n", osVaultName(), stackconfig.KeychainService, account)
	} else {
		fmt.Printf("vault:  none configured (set secrets.passphrase_command)\n")
	}

	if resolveErr != nil {
		fmt.Fprintln(os.Stderr, "sklp config passphrase status:", resolveErr)
		return 1
	}
	switch src {
	case stackconfig.SourceEnv:
		fmt.Printf("source: %s (env var takes precedence)\n", stackconfig.PassphraseEnv)
	case stackconfig.SourceNone:
		fmt.Printf("source: not found\n")
	default:
		fmt.Printf("source: %s\n", src)
	}

	return reportDecryption(pp)
}

// reportDecryption prints, per config file, whether pp actually opens it.
// Never the value, nor a prefix, nor its length: the point is to name the
// verdict, not to reveal any part of the secret.
func reportDecryption(pp string) int {
	paths, err := filepath.Glob(filepath.Join(".sklp", "*", "config.yaml"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp config passphrase:", err)
		return 1
	}
	if root := filepath.Join(".sklp", "config.yaml"); fileExists(root) {
		paths = append([]string{root}, paths...)
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		fmt.Println("files:  none found under .sklp/")
		return 0
	}

	failed := false
	for _, p := range paths {
		c, err := stackconfig.Load(p)
		if err != nil {
			fmt.Printf("  %-32s unreadable (%v)\n", p, err)
			failed = true
			continue
		}
		sealed := 0
		for _, k := range c.Keys() {
			if c.IsSecret(k) {
				sealed++
			}
		}
		if sealed == 0 {
			fmt.Printf("  %-32s no secrets\n", p)
			continue
		}
		if _, err := c.Resolve(pp); err != nil {
			fmt.Printf("  %-32s %d secret(s) — DOES NOT DECRYPT\n", p, sealed)
			failed = true
			continue
		}
		fmt.Printf("  %-32s %d secret(s) — ok\n", p, sealed)
	}
	if failed {
		return 1
	}
	return 0
}

func runPassphraseUnset() int {
	if !hasOSVault() {
		fmt.Fprintln(os.Stderr, "sklp config passphrase unset: no OS vault on this machine")
		return 2
	}
	account := stackconfig.RepoKey(".")
	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		cmd = exec.Command("security", "delete-generic-password",
			"-a", account, "-s", stackconfig.KeychainService)
	} else {
		cmd = exec.Command("secret-tool", "clear",
			"service", stackconfig.KeychainService, "account", account)
	}
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "sklp config passphrase unset: no vault entry for %q\n", account)
		return 1
	}
	fmt.Printf("removed the vault entry for %q\n", account)
	return 0
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// runPassphraseCommand persists the vault lookup command. It is validated by
// running it: a command stored blind fails much later, inside a pipeline,
// where the error reads as a wrong passphrase rather than a bad config line.
func runPassphraseCommand(cmdline string) int {
	cfg, path, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp config passphrase command:", err)
		return 1
	}
	if cmdline == "" {
		cfg.Secrets.PassphraseCommand = ""
		if err := cfg.Save(path); err != nil {
			fmt.Fprintln(os.Stderr, "sklp config passphrase command:", err)
			return 1
		}
		fmt.Printf("cleared secrets.passphrase_command in %s\n", path)
		return 0
	}
	cfg.Secrets.PassphraseCommand = cmdline
	if err := cfg.Save(path); err != nil {
		fmt.Fprintln(os.Stderr, "sklp config passphrase command:", err)
		return 1
	}
	fmt.Printf("set secrets.passphrase_command in %s\n", path)
	// Run the command directly rather than through ResolvePassphrase: the env
	// var outranks the vault there, so an operator who still exports the
	// passphrase would get "produced no passphrase" for a command that works.
	v, err := stackconfig.RunVaultCommand(cmdline, ".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: the command failed when run: %v\n", err)
	} else if v == "" {
		fmt.Fprintln(os.Stderr, "warning: the command produced no passphrase")
	}
	return 0
}
