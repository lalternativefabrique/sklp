package pipeline

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/stackconfig"
)

// writeStackConfig seeds a .sklp/config.yaml with the given plain and secret
// values, sealed under passphrase.
func writeStackConfig(t *testing.T, repo string, plain, secret map[string]string, passphrase string) string {
	t.Helper()
	// Isolate from the developer machine: without this, ResolvePassphrase
	// falls through to the USER-level ~/.config/sklp/config.yaml, and a dev
	// with a passphrase_command configured there gets different failures
	// than CI (which has no user config at all).
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := filepath.Join(repo, ".sklp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	c, err := stackconfig.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range plain {
		c.SetPlain(k, v)
	}
	for k, v := range secret {
		if err := c.SetSecret(k, v, passphrase); err != nil {
			t.Fatal(err)
		}
	}
	if err := stackconfig.Save(path, c); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStackConfigIsLoadedIntoTheRunEnv(t *testing.T) {
	repo := t.TempDir()
	writeStackConfig(t, repo,
		map[string]string{"PLAIN_VALUE": "visible"},
		map[string]string{"SEALED_VALUE": "decrypted-ok"},
		"test-passphrase")

	t.Setenv(stackconfig.PassphraseEnv, "test-passphrase")
	os.Unsetenv("PLAIN_VALUE")
	os.Unsetenv("SEALED_VALUE")

	var buf bytes.Buffer
	applied, err := LoadPipelineStackConfig(repo, []string{"SEALED_VALUE"}, &buf)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if applied["PLAIN_VALUE"] != "visible" {
		t.Errorf("plain value = %q, want visible", applied["PLAIN_VALUE"])
	}
	if applied["SEALED_VALUE"] != "decrypted-ok" {
		t.Errorf("secret value = %q, want decrypted-ok", applied["SEALED_VALUE"])
	}
	if os.Getenv("SEALED_VALUE") != "decrypted-ok" {
		t.Errorf("secret not injected into process env, got %q", os.Getenv("SEALED_VALUE"))
	}
	if !strings.Contains(buf.String(), "1 of 1 secrets decrypted") {
		t.Errorf("expected the log to report the encrypted count, got %q", buf.String())
	}
}

// The committed ciphertext must be worthless on its own: without the
// passphrase the run fails loudly rather than proceeding with an empty
// credential that only surfaces deep inside a step.
func TestStackConfigWithoutPassphraseFailsLoudly(t *testing.T) {
	repo := t.TempDir()
	writeStackConfig(t, repo, nil, map[string]string{"SEALED": "value"}, "the-passphrase")

	t.Setenv(stackconfig.PassphraseEnv, "")
	os.Unsetenv(stackconfig.PassphraseEnv)

	_, err := LoadPipelineStackConfig(repo, []string{"SEALED"}, nil)
	if err == nil {
		t.Fatal("a declared sealed value with no passphrase must fail, not silently skip")
	}
	if !strings.Contains(err.Error(), stackconfig.PassphraseEnv) {
		t.Fatalf("error should name %s, got: %v", stackconfig.PassphraseEnv, err)
	}
}

func TestStackConfigWrongPassphraseFails(t *testing.T) {
	repo := t.TempDir()
	writeStackConfig(t, repo, nil, map[string]string{"SEALED": "value"}, "right-one")

	t.Setenv(stackconfig.PassphraseEnv, "wrong-one")

	if _, err := LoadPipelineStackConfig(repo, []string{"SEALED"}, nil); err == nil {
		t.Fatal("a wrong passphrase must fail")
	}
}

// Host env outranks the file, matching env_files precedence: an explicitly
// exported value is never overridden by a checked-in config.
func TestStackConfigHostEnvWins(t *testing.T) {
	repo := t.TempDir()
	writeStackConfig(t, repo, map[string]string{"OVERRIDDEN": "from-file"}, nil, "")

	t.Setenv("OVERRIDDEN", "from-host")

	applied, err := LoadPipelineStackConfig(repo, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if applied["OVERRIDDEN"] != "from-host" {
		t.Fatalf("applied = %q, want the host value to win", applied["OVERRIDDEN"])
	}
	if os.Getenv("OVERRIDDEN") != "from-host" {
		t.Fatalf("host env was overwritten: %q", os.Getenv("OVERRIDDEN"))
	}
}

// A declared secret the host already exports — a project secret core injected
// on a CI node, or an operator's own export — must never be decrypted from
// the file: the run has no usable passphrase there, and asking for one would
// fail every remote run whose secrets come from core instead of this file.
func TestStackConfigDeclaredSecretAlreadyOnHostSkipsDecrypt(t *testing.T) {
	repo := t.TempDir()
	writeStackConfig(t, repo, nil, map[string]string{"API_KEY": "from-file"}, "correct-pass")

	os.Unsetenv(stackconfig.PassphraseEnv)
	t.Setenv("API_KEY", "from-host")

	applied, err := LoadPipelineStackConfig(repo, []string{"API_KEY"}, nil)
	if err != nil {
		t.Fatalf("LoadPipelineStackConfig: %v", err)
	}
	if applied["API_KEY"] != "from-host" {
		t.Fatalf("applied = %q, want the host value to win", applied["API_KEY"])
	}
	if os.Getenv("API_KEY") != "from-host" {
		t.Fatalf("host env was overwritten: %q", os.Getenv("API_KEY"))
	}
}

// `sklp config set --stack <s>` writes .sklp/<stack>/config.yaml. CI must read
// that exact path: a repo-root-only reader would silently ignore everything
// stored through the CLI, so a migrated repo would run with no secrets at all.
func TestPerStackConfigWrittenByTheCLIIsLoaded(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, ".sklp", "prod")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	c, err := stackconfig.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetSecret("STACK_SCOPED", "from-stack-dir", "pass"); err != nil {
		t.Fatal(err)
	}
	if err := stackconfig.Save(path, c); err != nil {
		t.Fatal(err)
	}

	t.Setenv(stackconfig.PassphraseEnv, "pass")
	os.Unsetenv("STACK_SCOPED")

	applied, err := LoadPipelineStackConfig(repo, []string{"STACK_SCOPED"}, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if applied["STACK_SCOPED"] != "from-stack-dir" {
		t.Fatalf("a .sklp/<stack>/config.yaml written by the CLI was not loaded: %v", applied)
	}
}

// A stack file may specialise a repo-wide value.
func TestPerStackConfigOverridesRepoWide(t *testing.T) {
	repo := t.TempDir()
	writeStackConfig(t, repo, map[string]string{"SHARED": "repo-wide"}, nil, "")

	dir := filepath.Join(repo, ".sklp", "prod")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	c, _ := stackconfig.Load(path)
	c.SetPlain("SHARED", "stack-specific")
	if err := stackconfig.Save(path, c); err != nil {
		t.Fatal(err)
	}

	os.Unsetenv("SHARED")
	applied, err := LoadPipelineStackConfig(repo, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if applied["SHARED"] != "stack-specific" {
		t.Fatalf("applied = %q, want the stack file to win", applied["SHARED"])
	}
}

// Host env must still outrank BOTH files. The per-stack override works by
// letting a later file replace what an earlier file placed — that must not
// extend to a value the operator exported themselves.
func TestHostEnvBeatsBothRepoWideAndPerStack(t *testing.T) {
	repo := t.TempDir()
	writeStackConfig(t, repo, map[string]string{"CONTESTED": "repo-wide"}, nil, "")

	dir := filepath.Join(repo, ".sklp", "prod")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	c, _ := stackconfig.Load(path)
	c.SetPlain("CONTESTED", "stack-specific")
	if err := stackconfig.Save(path, c); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONTESTED", "from-host")

	applied, err := LoadPipelineStackConfig(repo, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if applied["CONTESTED"] != "from-host" {
		t.Fatalf("applied = %q, want the host value to win over both files", applied["CONTESTED"])
	}
	if os.Getenv("CONTESTED") != "from-host" {
		t.Fatalf("host env was overwritten: %q", os.Getenv("CONTESTED"))
	}
}

// The file is opt-in; a repo still on env_files only must run untouched.
func TestMissingStackConfigIsANoOp(t *testing.T) {
	applied, err := LoadPipelineStackConfig(t.TempDir(), nil, nil)
	if err != nil {
		t.Fatalf("a missing config must not error: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("expected no values, got %v", applied)
	}
}

// End-to-end through a real pipeline run: a sealed value reaches a step that
// declares it, exactly like any other secret.
func TestStackConfigSecretReachesDeclaringStep(t *testing.T) {
	t.Setenv(stackconfig.PassphraseEnv, "e2e-passphrase")

	res, stdout, _, err := runPipelineFromYAML(t, runPipelineCase{
		setup: func(t *testing.T, repo string) {
			writeStackConfig(t, repo, nil,
				map[string]string{"SEALED_TOKEN": "from-encrypted-config"},
				"e2e-passphrase")
		},
		yaml: `version: 1
kind: pipeline
name: stackconfig-e2e

secrets:
  - SEALED_TOKEN

steps:
  - name: uses-sealed-secret
    secrets: [SEALED_TOKEN]
    run: echo "seen=[${SEALED_TOKEN}]"
`,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.PassFail != "pass" {
		t.Fatalf("pipeline failed: %s", stdout)
	}
	if !strings.Contains(stdout, "seen=[from-encrypted-config]") {
		t.Fatalf("sealed value did not reach the step:\n%s", stdout)
	}
}

// A pipeline that declares no secret must run without the vault passphrase:
// the encrypted entries belong to the deploy stacks, and asking a lint/test
// run for the key to production is exactly the coupling this avoids. Plain
// values from the same file still load.
func TestUndeclaredSecretsNeedNoPassphrase(t *testing.T) {
	repo := t.TempDir()
	writeStackConfig(t, repo,
		map[string]string{"PLAIN_VALUE": "visible"},
		map[string]string{"DEPLOY_ONLY_KEY": "prod-secret"},
		"the-passphrase")

	t.Setenv(stackconfig.PassphraseEnv, "")
	os.Unsetenv(stackconfig.PassphraseEnv)
	os.Unsetenv("PLAIN_VALUE")
	os.Unsetenv("DEPLOY_ONLY_KEY")

	applied, err := LoadPipelineStackConfig(repo, nil, nil)
	if err != nil {
		t.Fatalf("a pipeline declaring no secret must not need the passphrase: %v", err)
	}
	if applied["PLAIN_VALUE"] != "visible" {
		t.Errorf("plain value = %q, want visible", applied["PLAIN_VALUE"])
	}
	if _, ok := applied["DEPLOY_ONLY_KEY"]; ok {
		t.Error("an undeclared secret must stay sealed, not be injected")
	}
}
