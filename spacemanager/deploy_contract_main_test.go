package spacemanager

import (
	"os"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/stackconfig"
)

// TestMain resolves the repo's real secrets passphrase (env, configured
// command, or OS vault — see stackconfig.ResolvePassphrase) before the deploy
// contract tests run: Load decrypts every secret in .sklp/skalpai/config.yaml
// up front, so a wrong passphrase fails at decryption before a test's own
// stubbed values (host env, which always wins) ever come into play. A machine
// with no vault (CI without SKLP_SECRETS_PASSPHRASE) leaves the env untouched;
// the contract tests then skip via their own os.Stat guard only if the deploy
// file itself is absent — a decrypt failure still surfaces as a clear error.
func TestMain(m *testing.M) {
	if os.Getenv("SKLP_SECRETS_PASSPHRASE") == "" {
		if pp, _, err := stackconfig.ResolvePassphrase(repoRootForTests()); err == nil && pp != "" {
			os.Setenv("SKLP_SECRETS_PASSPHRASE", pp)
		}
	}
	os.Exit(m.Run())
}

func repoRootForTests() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd + "/.."
}
