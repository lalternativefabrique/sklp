package space

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/registry"
)

func writeRegistries(t *testing.T, body string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sklp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "registries.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write registries.yaml: %v", err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Dir(dir))
}

func TestRegistryAuths_MissingFileReturnsEmpty(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	user, pass, err := registryAuths(nil)("rg.fr-par.scw.cloud")
	if err != nil {
		t.Fatalf("auth fn: %v", err)
	}
	if user != "" || pass != "" {
		t.Errorf("no stored credential should yield empty creds, got %q/%q", user, pass)
	}
}

func TestRegistryAuths_ReadsStoredCredential(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString([]byte("nologin:s3cret"))
	writeRegistries(t, "version: 1\nregistries:\n  rg.fr-par.scw.cloud:\n    username: nologin\n    auth: "+enc+"\n")

	user, pass, err := registryAuths(nil)("rg.fr-par.scw.cloud")
	if err != nil {
		t.Fatalf("auth fn: %v", err)
	}
	if user != "nologin" || pass != "s3cret" {
		t.Errorf("got %q/%q, want nologin/s3cret", user, pass)
	}
}

// ~/.docker/config.json is not an input. A credential left there by a past
// `docker login` — possibly revoked — must not authenticate an sklp pull or
// push, since sklp never saw it and cannot report on it.
func TestRegistryAuths_IgnoresDockerConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dockerDir := filepath.Join(home, ".docker")
	if err := os.MkdirAll(dockerDir, 0o755); err != nil {
		t.Fatalf("mkdir .docker: %v", err)
	}
	enc := base64.StdEncoding.EncodeToString([]byte("stale:revoked"))
	body := `{"auths":{"rg.fr-par.scw.cloud":{"auth":"` + enc + `"}}}`
	if err := os.WriteFile(filepath.Join(dockerDir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	user, pass, err := registryAuths(nil)("rg.fr-par.scw.cloud")
	if err != nil {
		t.Fatalf("auth fn: %v", err)
	}
	if user != "" || pass != "" {
		t.Fatalf("docker config must not be consulted, got %q/%q", user, pass)
	}
}

// Deploy-supplied credentials (a deploy.yaml `registries:` block, carried in
// memory on the context) win over the stored ones for the hosts they name.
func TestRegistryAuths_OverridesWinOverStored(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString([]byte("nologin:stored"))
	writeRegistries(t, "version: 1\nregistries:\n  rg.fr-par.scw.cloud:\n    username: nologin\n    auth: "+enc+"\n")

	overrides := map[string]RegistryAuth{
		"rg.fr-par.scw.cloud": {Username: "deploy", Password: "from-deploy"},
	}
	user, pass, err := registryAuths(overrides)("rg.fr-par.scw.cloud")
	if err != nil {
		t.Fatalf("auth fn: %v", err)
	}
	if user != "deploy" || pass != "from-deploy" {
		t.Errorf("got %q/%q, want the deploy-supplied pair", user, pass)
	}
}

// A credential the run resolved is already materialised, so it answers for
// its host without re-reading the file.
func TestRegistryAuths_RememberedCredentialWins(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Cleanup(registry.ForgetAll)
	registry.ForgetAll()
	registry.Remember("ghcr.io", "oauth2", "tok")

	user, pass, err := registryAuths(nil)("ghcr.io")
	if err != nil {
		t.Fatalf("auth fn: %v", err)
	}
	if user != "oauth2" || pass != "tok" {
		t.Errorf("got %q/%q, want oauth2/tok", user, pass)
	}
}

func TestWithRegistryAuths_EmptyIsNoOp(t *testing.T) {
	ctx := context.Background()
	if got := WithRegistryAuths(ctx, nil); got != ctx {
		t.Error("nil auths should return the input context unchanged")
	}
	if registryAuthsFrom(ctx) != nil {
		t.Error("a bare context carries no auths")
	}
}

func TestWithRegistryAuths_RoundTrips(t *testing.T) {
	in := map[string]RegistryAuth{"ghcr.io": {Username: "u", Password: "p"}}
	got := registryAuthsFrom(WithRegistryAuths(context.Background(), in))
	if got["ghcr.io"].Username != "u" || got["ghcr.io"].Password != "p" {
		t.Errorf("round trip lost the auths: %+v", got)
	}
}
