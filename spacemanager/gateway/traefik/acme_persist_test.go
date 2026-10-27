package traefik

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/ingress"
)

func testACME() ingress.ACMEConfig {
	return ingress.ACMEConfig{
		Email:              "ops@example.com",
		DNSProvider:        "ovh",
		DNSCredentials:     map[string]string{"OVH_APPLICATION_KEY": "secret-key", "OVH_CONSUMER_KEY": "secret-consumer"},
		PropagationTimeout: 120 * time.Second,
	}
}

// The whole point of the fix: a relaunch with no SKLP_SPACE_ACME must still
// resolve the dns provider a deploy configured, instead of downgrading to
// httpChallenge.
func TestResolvedACME_FallsBackToPersistedConfig(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "traefik.yaml")
	saveACMEConfig(cfgPath, testACME())

	t.Setenv(ingress.EnvACME, "")
	t.Setenv("OVH_APPLICATION_KEY", "secret-key")
	t.Setenv("OVH_CONSUMER_KEY", "secret-consumer")

	got, ok := resolvedACME(cfgPath)
	if !ok {
		t.Fatal("persisted config must be used when the env var is absent")
	}
	if got.DNSProvider != "ovh" {
		t.Errorf("DNSProvider = %q, want ovh", got.DNSProvider)
	}
	if got.DNSCredentials["OVH_APPLICATION_KEY"] != "secret-key" {
		t.Error("credential values must be refilled from the environment")
	}
	if got.PropagationTimeout != 120*time.Second {
		t.Errorf("PropagationTimeout = %v, want 2m", got.PropagationTimeout)
	}
}

// Credential VALUES must never reach the disk — only the names of the env vars
// holding them, as deploy.yaml itself requires.
func TestSaveACMEConfig_NeverWritesSecrets(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "traefik.yaml")
	saveACMEConfig(cfgPath, testACME())

	b, err := os.ReadFile(acmeConfigPath(cfgPath))
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	body := string(b)
	for _, secret := range []string{"secret-key", "secret-consumer"} {
		if strings.Contains(body, secret) {
			t.Fatalf("persisted config leaked a credential value: %s", body)
		}
	}
	if !strings.Contains(body, "OVH_APPLICATION_KEY") {
		t.Errorf("persisted config must record the credential var NAMES: %s", body)
	}
}

// The env var stays authoritative: a deploy carrying fresh settings must not be
// overridden by an older file on disk.
func TestResolvedACME_EnvWinsOverPersisted(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "traefik.yaml")
	saveACMEConfig(cfgPath, testACME())

	fresh := testACME()
	fresh.DNSProvider = "cloudflare"
	env, err := ingress.MarshalACMEEnv(fresh)
	if err != nil {
		t.Fatalf("MarshalACMEEnv: %v", err)
	}
	t.Setenv(ingress.EnvACME, env)

	got, ok := resolvedACME(cfgPath)
	if !ok || got.DNSProvider != "cloudflare" {
		t.Fatalf("DNSProvider = %q (ok=%v), want cloudflare from the env", got.DNSProvider, ok)
	}
}

// A dnsChallenge whose provider cannot authenticate fails every issuance, where
// httpChallenge still serves the non-wildcard hosts — so missing credentials
// must NOT resolve.
func TestResolvedACME_MissingCredentialsDoesNotResolve(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "traefik.yaml")
	saveACMEConfig(cfgPath, testACME())

	t.Setenv(ingress.EnvACME, "")
	t.Setenv("OVH_APPLICATION_KEY", "secret-key")
	os.Unsetenv("OVH_CONSUMER_KEY")

	if _, ok := resolvedACME(cfgPath); ok {
		t.Fatal("must not resolve when a named credential is absent from the environment")
	}
	if acmeConfigError(cfgPath) == nil {
		t.Error("the operator must be told why the gateway fell back to httpChallenge")
	}
}

// No ACME configured at all stays the silent, supported case.
func TestResolvedACME_NothingConfigured(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "traefik.yaml")
	t.Setenv(ingress.EnvACME, "")

	if _, ok := resolvedACME(cfgPath); ok {
		t.Fatal("must not resolve with neither env nor persisted config")
	}
	if err := acmeConfigError(cfgPath); err != nil {
		t.Errorf("no warning is due when nothing was ever persisted: %v", err)
	}
}

// httpChallenge setups (no dns provider) persist nothing: there is no wildcard
// to protect and no credential to name.
func TestSaveACMEConfig_SkipsWithoutDNSProvider(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "traefik.yaml")
	saveACMEConfig(cfgPath, ingress.ACMEConfig{Email: "ops@example.com"})

	if _, err := os.Stat(acmeConfigPath(cfgPath)); !os.IsNotExist(err) {
		t.Error("an httpChallenge setup must not persist an ACME config file")
	}
}

// The rendered static config is what actually decides the challenge, so assert
// the end result a relaunch produces.
func TestRenderStaticConfig_DNSChallengeFromPersisted(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "traefik.yaml")
	saveACMEConfig(cfgPath, testACME())

	t.Setenv(ingress.EnvACME, "")
	t.Setenv("OVH_APPLICATION_KEY", "secret-key")
	t.Setenv("OVH_CONSUMER_KEY", "secret-consumer")

	acme, _ := resolvedACME(cfgPath)
	out := renderStaticConfig("/dyn", "/acme.json", "/traefik.log", "ops@example.com", acme.DNSProvider)
	if !strings.Contains(out, "dnsChallenge") || !strings.Contains(out, "provider: ovh") {
		t.Fatalf("relaunch must render a dnsChallenge, got:\n%s", out)
	}
	if strings.Contains(out, "httpChallenge") {
		t.Fatal("httpChallenge cannot issue a wildcard — it must not be rendered here")
	}
}
