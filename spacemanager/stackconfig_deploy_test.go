package spacemanager

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/stackconfig"
)

// TestDeployResolvesSecretFromConfig: a ${VAR} in the deploy file resolves from
// the stack's config.yaml secret (decrypted), not requiring it in the env.
func TestDeployResolvesSecretFromConfig(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, ".sklp", "prod")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Write a config.yaml with an encrypted secret.
	t.Setenv(stackconfig.PassphraseEnv, "pw")
	c := &stackconfig.Config{Plain: map[string]string{}, Secret: map[string]string{}}
	if err := c.SetSecret("MY_SECRET", "resolved-from-config", "pw"); err != nil {
		t.Fatal(err)
	}
	if err := stackconfig.Save(filepath.Join(stackDir, "config.yaml"), c); err != nil {
		t.Fatal(err)
	}
	// A deploy file referencing ${MY_SECRET}; the var is NOT in the env.
	os.Unsetenv("MY_SECRET")
	deployYAML := `services:
  api:
    image: img:1
    env:
      TOKEN: ${MY_SECRET}
spaces:
  production: {}
`
	dp := filepath.Join(stackDir, "deploy.yaml")
	if err := os.WriteFile(dp, []byte(deployYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dp)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Services["api"].Env["TOKEN"]; got != "resolved-from-config" {
		t.Fatalf("TOKEN = %q, want resolved-from-config (from config.yaml secret)", got)
	}
}

// TestDeployMergesRepoWideConfig: a value shared by every stack lives once in
// .sklp/config.yaml, and the stack's own file specialises it. Without the
// repo-wide layer each stack has to carry a copy of every shared secret.
func TestDeployMergesRepoWideConfig(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, ".sklp", "prod")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(stackconfig.PassphraseEnv, "pw")

	repo := &stackconfig.Config{Plain: map[string]string{}, Secret: map[string]string{}}
	repo.SetPlain("SHARED", "from-repo")
	repo.SetPlain("OVERRIDDEN", "from-repo")
	if err := repo.SetSecret("SHARED_SECRET", "repo-secret", "pw"); err != nil {
		t.Fatal(err)
	}
	if err := stackconfig.Save(filepath.Join(root, ".sklp", "config.yaml"), repo); err != nil {
		t.Fatal(err)
	}

	stack := &stackconfig.Config{Plain: map[string]string{}, Secret: map[string]string{}}
	stack.SetPlain("OVERRIDDEN", "from-stack")
	if err := stackconfig.Save(filepath.Join(stackDir, "config.yaml"), stack); err != nil {
		t.Fatal(err)
	}

	for _, k := range []string{"SHARED", "OVERRIDDEN", "SHARED_SECRET"} {
		os.Unsetenv(k)
	}
	deployYAML := `services:
  api:
    image: img:1
    env:
      A: ${SHARED}
      B: ${OVERRIDDEN}
      C: ${SHARED_SECRET}
spaces:
  production: {}
`
	dp := filepath.Join(stackDir, "deploy.yaml")
	if err := os.WriteFile(dp, []byte(deployYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dp)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	env := cfg.Services["api"].Env
	if got := env["A"]; got != "from-repo" {
		t.Errorf("A = %q, want from-repo (repo-wide plain value)", got)
	}
	if got := env["B"]; got != "from-stack" {
		t.Errorf("B = %q, want from-stack (stack file specialises the repo-wide value)", got)
	}
	if got := env["C"]; got != "repo-secret" {
		t.Errorf("C = %q, want repo-secret (repo-wide secret decrypted)", got)
	}
}

// TestDeployHostEnvBeatsBothConfigs: an operator export outranks both files.
// The snapshot matters here — the repo-wide file is injected first, so a naive
// os.LookupEnv check would mistake its own write for a host export and let the
// stack file win over the operator.
func TestDeployHostEnvBeatsBothConfigs(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, ".sklp", "prod")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(stackconfig.PassphraseEnv, "pw")
	t.Setenv("PINNED", "from-host")

	repo := &stackconfig.Config{Plain: map[string]string{}, Secret: map[string]string{}}
	repo.SetPlain("PINNED", "from-repo")
	if err := stackconfig.Save(filepath.Join(root, ".sklp", "config.yaml"), repo); err != nil {
		t.Fatal(err)
	}
	stack := &stackconfig.Config{Plain: map[string]string{}, Secret: map[string]string{}}
	stack.SetPlain("PINNED", "from-stack")
	if err := stackconfig.Save(filepath.Join(stackDir, "config.yaml"), stack); err != nil {
		t.Fatal(err)
	}

	deployYAML := `services:
  api:
    image: img:1
    env:
      A: ${PINNED}
spaces:
  production: {}
`
	dp := filepath.Join(stackDir, "deploy.yaml")
	if err := os.WriteFile(dp, []byte(deployYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dp)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Services["api"].Env["A"]; got != "from-host" {
		t.Fatalf("A = %q, want from-host (an operator export outranks both files)", got)
	}
}
