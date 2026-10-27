package spacemanager

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/registry"
)

// TestParse_Registries_ResolvesPasswordButKeepsEnvName proves the secret-safe
// contract: a ${VAR} password is expanded into the in-memory config (so a pull
// at deploy time works) AND its env var NAME is captured for the manifest (so a
// reboot re-resolves it without the value ever being persisted).
func TestParse_Registries_ResolvesPasswordButKeepsEnvName(t *testing.T) {
	t.Setenv("S3_SECRET_KEY", "s3cr3t-value")
	yaml := "" +
		"services:\n" +
		"  api:\n" +
		"    image: rg.fr-par.scw.cloud/shop/core:latest\n" +
		"registries:\n" +
		"  rg.fr-par.scw.cloud:\n" +
		"    username: nologin\n" +
		"    password: ${S3_SECRET_KEY}\n"

	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	spec, ok := cfg.Registries["rg.fr-par.scw.cloud"]
	if !ok {
		t.Fatal("registry entry not parsed")
	}
	if spec.Username != "nologin" {
		t.Errorf("username = %q, want nologin", spec.Username)
	}
	// In-memory password is expanded (needed for the deploy-time pull).
	if spec.Password != "s3cr3t-value" {
		t.Errorf("in-memory password = %q, want the expanded value", spec.Password)
	}
	// But the captured env name is the VAR, so the manifest persists only that.
	if got := cfg.registryPasswordEnv["rg.fr-par.scw.cloud"]; got != "S3_SECRET_KEY" {
		t.Errorf("captured password env = %q, want S3_SECRET_KEY", got)
	}
}

// TestRegistryAuthMeta_IsSecretSafe asserts the manifest metadata carries the
// env var NAME, never the password value.
func TestRegistryAuthMeta_IsSecretSafe(t *testing.T) {
	t.Setenv("S3_SECRET_KEY", "do-not-persist-me")
	cfg, err := Parse([]byte("services:\n  api:\n    image: r/x:1\nregistries:\n  reg.example:\n    username: u\n    password: ${S3_SECRET_KEY}\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	meta := cfg.registryAuthMeta()
	m, ok := meta["reg.example"]
	if !ok {
		t.Fatal("no meta for reg.example")
	}
	if m.PasswordEnv != "S3_SECRET_KEY" {
		t.Errorf("PasswordEnv = %q, want S3_SECRET_KEY", m.PasswordEnv)
	}
	// The value must NOT appear anywhere in the persisted meta struct.
	if m.Username == "do-not-persist-me" {
		t.Error("password value leaked into username")
	}
}

// TestResolveRegistryAuths_ReadsEnvAtLaunch proves the launch-time resolver
// turns the secret-safe metadata back into a live credential by reading the env.
func TestResolveRegistryAuths_ReadsEnvAtLaunch(t *testing.T) {
	t.Setenv("MY_REG_PW", "live-pw")
	metas := map[string]RegistryAuthMeta{
		"reg.example": {Username: "robot", PasswordEnv: "MY_REG_PW"},
	}
	got := ResolveRegistryAuths(metas)
	a, ok := got["reg.example"]
	if !ok {
		t.Fatal("reg.example not resolved")
	}
	if a.Username != "robot" || a.Password != "live-pw" {
		t.Errorf("resolved = %+v, want {robot live-pw}", a)
	}
}

// TestResolveRegistryAuths_SkipsUnsetEnv: a registry whose password env is unset
// is dropped (so the pull falls back to anonymous and fails loudly, rather than
// authenticating with an empty password).
func TestResolveRegistryAuths_SkipsUnsetEnv(t *testing.T) {
	os.Unsetenv("DEFINITELY_UNSET_PW")
	got := ResolveRegistryAuths(map[string]RegistryAuthMeta{
		"reg.example": {Username: "u", PasswordEnv: "DEFINITELY_UNSET_PW"},
	})
	if len(got) != 0 {
		t.Errorf("unset env should yield no creds, got %+v", got)
	}
}

// TestParse_Registries_Validation: missing username/password is a hard error so
// a misconfigured (or blank-env) credential fails at load, not at pull.
func TestParse_Registries_Validation(t *testing.T) {
	cases := map[string]string{
		"missing username": "services:\n  a:\n    image: i:1\nregistries:\n  r:\n    password: p\n",
		"missing password": "services:\n  a:\n    image: i:1\nregistries:\n  r:\n    username: u\n",
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(yaml)); err == nil {
				t.Errorf("%s: want error, got nil", name)
			}
		})
	}
}

func loginFileWith(t *testing.T, host, user, password string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registries.yaml")
	body := "version: 1\nregistries:\n  " + host + ":\n    username: " + user + "\n    auth: " + registry.EncodeAuth(user, password) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(registry.EnvFile, path)
}

const loginFallbackYAML = "" +
	"services:\n" +
	"  api:\n" +
	"    image: rg.fr-par.scw.cloud/shop/core:1\n" +
	"registries:\n" +
	"  rg.fr-par.scw.cloud:\n" +
	"    username: nologin\n" +
	"    password: ${SKLP_TEST_REG_PW}\n"

// A laptop that ran `sklp registry login` deploys without exporting the
// registry secret: the block's variable is taken from the login file.
func TestParse_Registries_FallsBackToLoginFile(t *testing.T) {
	os.Unsetenv("SKLP_TEST_REG_PW")
	loginFileWith(t, "rg.fr-par.scw.cloud", "nologin", "from-login")
	cfg, err := Parse([]byte(loginFallbackYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Registries["rg.fr-par.scw.cloud"].Password; got != "from-login" {
		t.Fatalf("password = %q, want the login file's", got)
	}
	if got := cfg.registryPasswordEnv["rg.fr-par.scw.cloud"]; got != "SKLP_TEST_REG_PW" {
		t.Fatalf("captured env = %q: the manifest must still persist the variable name", got)
	}
}

func TestParse_Registries_EnvWinsOverLoginFile(t *testing.T) {
	t.Setenv("SKLP_TEST_REG_PW", "from-env")
	loginFileWith(t, "rg.fr-par.scw.cloud", "nologin", "from-login")
	cfg, err := Parse([]byte(loginFallbackYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Registries["rg.fr-par.scw.cloud"].Password; got != "from-env" {
		t.Fatalf("password = %q, want the environment's", got)
	}
}

// Only a registry password variable may come from the login file; any other
// unset variable is still the hard error it always was.
func TestParse_Registries_LoginFileCoversOnlyRegistryVars(t *testing.T) {
	os.Unsetenv("SKLP_TEST_REG_PW")
	loginFileWith(t, "other.example", "u", "p")
	if _, err := Parse([]byte(loginFallbackYAML)); err == nil {
		t.Fatal("a host the login file does not cover must still fail")
	}
	t.Setenv("SKLP_TEST_REG_PW", "x")
	os.Unsetenv("SKLP_TEST_OTHER")
	yaml := loginFallbackYAML + "    extra: ${SKLP_TEST_OTHER}\n"
	if _, err := Parse([]byte(yaml)); err == nil {
		t.Fatal("an unrelated unset variable must still fail")
	}
}

// The remote-apply client ships the login file's password when the operator
// has not exported the variable, so the node pulls exactly as it would have.
func TestResolveRegistryPasswords_FallsBackToLoginFile(t *testing.T) {
	os.Unsetenv("SKLP_TEST_REG_PW")
	loginFileWith(t, "rg.fr-par.scw.cloud", "nologin", "from-login")
	out := ResolveRegistryPasswords(map[string]RegistryAuthMeta{
		"rg.fr-par.scw.cloud": {Username: "nologin", PasswordEnv: "SKLP_TEST_REG_PW"},
	})
	if got := out["rg.fr-par.scw.cloud"].Password; got != "from-login" {
		t.Fatalf("password = %q, want the login file's", got)
	}
}
