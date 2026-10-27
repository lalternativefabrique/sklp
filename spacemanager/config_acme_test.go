package spacemanager

import (
	"os"
	"strings"
	"testing"
)

const acmeBase = "gateway:\n  email: ops@example.com\n  acme:\n"

func TestParse_ACMEDisabledByDefault(t *testing.T) {
	y := "services:\n  core:\n    image: img\n"
	cfg, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.ACMEEnabled() {
		t.Fatal("ACME should be disabled without a gateway.acme block")
	}
}

func TestParse_ACMEEnabledOK(t *testing.T) {
	t.Setenv("OVH_APPLICATION_KEY", "ak")
	t.Setenv("OVH_APPLICATION_SECRET", "as")
	t.Setenv("OVH_CONSUMER_KEY", "ck")
	y := acmeBase + "    enabled: true\n    dns_provider: ovh\n    staging: true\nservices:\n  core:\n    image: img\n"
	cfg, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !cfg.ACMEEnabled() {
		t.Fatal("ACME should be enabled")
	}
	rc, ok := cfg.ResolvedACMEConfig()
	if !ok {
		t.Fatal("ResolvedACMEConfig not ok")
	}
	if rc.Email != "ops@example.com" || rc.DNSProvider != "ovh" || !rc.Staging {
		t.Fatalf("resolved config wrong: %+v", rc)
	}
	if rc.StorageDir != ACMEStorageDir() {
		t.Fatalf("storage should default, got %q", rc.StorageDir)
	}
	if rc.DNSCredentials["OVH_APPLICATION_KEY"] != "ak" {
		t.Fatalf("creds not resolved from env: %+v", rc.DNSCredentials)
	}
}

func TestParse_ACMEAllowedInSingletonMode(t *testing.T) {
	// singleton (default) + acme enabled is valid: the traefik driver consumes
	// gateway.acme via its own dnsChallenge, same as the native per-space L7.
	y := "gateway:\n  email: ops@example.com\n  acme:\n    enabled: true\n    dns_provider: ovh\nservices:\n  core:\n    image: img\n"
	if _, err := Parse([]byte(y)); err != nil {
		t.Fatalf("expected singleton-mode acme to parse cleanly, got %v", err)
	}
}

// TestResolve_ACMEAllowedInBothModes: gateway.acme no longer restricts the
// node's gateway mode — both the native per-space L7 and the traefik
// singleton solve dns-01 (see validateGatewayModeACME in space.go).
func TestResolve_ACMEAllowedInBothModes(t *testing.T) {
	t.Setenv("OVH_APPLICATION_KEY", "ak")
	t.Setenv("OVH_APPLICATION_SECRET", "as")
	t.Setenv("OVH_CONSUMER_KEY", "ck")
	y := "gateway:\n  email: ops@example.com\n  acme:\n    enabled: true\n    dns_provider: ovh\nservices:\n  core:\n    image: img\n"
	cfg, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, _, err := cfg.Resolve("production", GatewayModeSingleton); err != nil {
		t.Fatalf("Resolve(singleton): unexpected error: %v", err)
	}
	if _, _, err := cfg.Resolve("production", GatewayModePerSpace); err != nil {
		t.Fatalf("Resolve(per-space): unexpected error: %v", err)
	}
}

func TestParse_ACMEUnknownProviderIsError(t *testing.T) {
	y := acmeBase + "    enabled: true\n    dns_provider: nope\nservices:\n  core:\n    image: img\n"
	_, err := Parse([]byte(y))
	if err == nil || !strings.Contains(err.Error(), "dns_provider") {
		t.Fatalf("expected unknown provider error, got %v", err)
	}
}

func TestParse_ACMERequiresProvider(t *testing.T) {
	y := acmeBase + "    enabled: true\nservices:\n  core:\n    image: img\n"
	_, err := Parse([]byte(y))
	if err == nil || !strings.Contains(err.Error(), "dns_provider") {
		t.Fatalf("expected missing provider error, got %v", err)
	}
}

// TestQA_ParseStaleACMEEnvAfterDisable proves a re-parse where ACME was removed
// UNSETS SKLP_SPACE_ACME, so a long-lived supervisor never mints certs with
// stale creds and never leaks one stack's creds into another's workers.
func TestQA_ParseStaleACMEEnvAfterDisable(t *testing.T) {
	t.Setenv("OVH_APPLICATION_KEY", "ak")
	t.Setenv("OVH_APPLICATION_SECRET", "as")
	t.Setenv("OVH_CONSUMER_KEY", "ck")
	// First parse: ACME enabled -> env is set.
	enabled := acmeBase + "    enabled: true\n    dns_provider: ovh\nservices:\n  core:\n    image: img\n"
	if _, err := Parse([]byte(enabled)); err != nil {
		t.Fatalf("parse enabled: %v", err)
	}
	if v := os.Getenv("SKLP_SPACE_ACME"); v == "" {
		t.Fatal("expected SKLP_SPACE_ACME set after enabling ACME")
	}
	// Second parse: ACME removed -> env must be cleared.
	disabled := "gateway:\n  email: ops@example.com\nservices:\n  core:\n    image: img\n"
	if _, err := Parse([]byte(disabled)); err != nil {
		t.Fatalf("parse disabled: %v", err)
	}
	if v := os.Getenv("SKLP_SPACE_ACME"); v != "" {
		t.Fatalf("SKLP_SPACE_ACME must be unset after disabling ACME, got %q", v)
	}
}

func TestParse_ACMERequiresEmail(t *testing.T) {
	t.Setenv("SKLP_GATEWAY_ACME_EMAIL", "")
	y := "gateway:\n  acme:\n    enabled: true\n    dns_provider: ovh\nservices:\n  core:\n    image: img\n"
	_, err := Parse([]byte(y))
	if err == nil || !strings.Contains(err.Error(), "email") {
		t.Fatalf("expected missing email error, got %v", err)
	}
}

func TestParse_ACMEStorageDirIsNotAManifestField(t *testing.T) {
	y := acmeBase + "    enabled: true\n    dns_provider: ovh\n    storage_dir: /home/architect/.config/sklp\nservices:\n  core:\n    image: img\n"
	if _, err := Parse([]byte(y)); err == nil || !strings.Contains(err.Error(), "storage_dir") {
		t.Fatalf("want storage_dir rejected, got %v", err)
	}
}
