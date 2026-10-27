package main

import (
	"testing"

	"github.com/lalternativefabrique/sklp/internal/config"
)

// withCleanConfig points the config at a temp dir and clears the env overrides
// so each test starts from a pristine context store.
func withCleanConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("SKLP_CONFIG", "")
	t.Setenv(config.EnvContext, "")
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvContextToken, "")
}

// TestDeployLoginWritesContext: `deploy login` upserts server+token+fingerprint
// into a deploy context, persists it, and makes it current since none was.
func TestDeployLoginWritesContext(t *testing.T) {
	withCleanConfig(t)

	if err := deployLogin("", "https://vm:9443", "tok-123", "fp-abc", ""); err != nil {
		t.Fatalf("deployLogin: %v", err)
	}

	cfg, _, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	c, ok := cfg.Contexts["default"]
	if !ok {
		t.Fatalf("default context not written; contexts=%+v", cfg.Contexts)
	}
	if c.Server != "https://vm:9443" || c.Token != "tok-123" || c.CAFingerprint != "fp-abc" {
		t.Fatalf("context not set correctly: %+v", c)
	}
	if cfg.CurrentContext != "default" {
		t.Fatalf("CurrentContext = %q, want default (first context becomes current)", cfg.CurrentContext)
	}
	// Deploy login must not touch the SaaS block.
	if cfg.API.URL != "" || cfg.API.Token != "" {
		t.Fatalf("deploy login wrote into the api block: %+v", cfg.API)
	}
}

// TestDeployLoginRequiresServerAndToken: missing server or token is an error.
func TestDeployLoginRequiresServerAndToken(t *testing.T) {
	withCleanConfig(t)
	if err := deployLogin("", "", "tok", "fp", ""); err == nil {
		t.Fatal("expected error for empty --server")
	}
	if err := deployLogin("", "https://vm:9443", "", "fp", ""); err == nil {
		t.Fatal("expected error for empty --token")
	}
}

// TestDeployLoginKeepsSpaceOnReLogin: re-running login with only a fresh token
// must not wipe the space already recorded for that context.
func TestDeployLoginKeepsSpaceOnReLogin(t *testing.T) {
	withCleanConfig(t)
	if err := deployLogin("", "https://vm:9443", "tok-1", "fp", "production"); err != nil {
		t.Fatalf("deployLogin: %v", err)
	}
	if err := deployLogin("", "https://vm:9443", "tok-2", "fp", ""); err != nil {
		t.Fatalf("deployLogin (re-login): %v", err)
	}
	cfg, _, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	c := cfg.Contexts["default"]
	if c.Space != "production" {
		t.Fatalf("space lost on re-login: %+v", c)
	}
	if c.Token != "tok-2" {
		t.Fatalf("token not refreshed: %+v", c)
	}
}

// TestNetFlagsResolveFromContext: with a logged-in context and no flags, the
// net flags pick up server, token, and ca-fingerprint from the active context.
func TestNetFlagsResolveFromContext(t *testing.T) {
	withCleanConfig(t)
	if err := deployLogin("", "https://vm:9443", "tok-xyz", "fp-pin", ""); err != nil {
		t.Fatalf("deployLogin: %v", err)
	}

	nf := &netFlags{}
	nf.applyContextDefaults()

	if nf.server != "https://vm:9443" {
		t.Fatalf("server = %q, want from context", nf.server)
	}
	if nf.caFinger != "fp-pin" {
		t.Fatalf("ca-fingerprint = %q, want from context", nf.caFinger)
	}
	if nf.token != "tok-xyz" {
		t.Fatalf("token = %q, want from context", nf.token)
	}
	if !nf.remote() {
		t.Fatal("expected remote() true after context resolution")
	}
}

// TestNetFlagsExplicitFlagsWinOverContext: an explicit --server is not
// overwritten by the context.
func TestNetFlagsExplicitFlagsWinOverContext(t *testing.T) {
	withCleanConfig(t)
	if err := deployLogin("", "https://ctx:9443", "ctok", "cfp", ""); err != nil {
		t.Fatalf("deployLogin: %v", err)
	}

	nf := &netFlags{server: "https://explicit:9443"}
	nf.applyContextDefaults()

	if nf.server != "https://explicit:9443" {
		t.Fatalf("explicit server overwritten: %q", nf.server)
	}
	// The other empty fields still fall back to the context.
	if nf.caFinger != "cfp" || nf.token != "ctok" {
		t.Fatalf("context fallback failed: caFinger=%q token=%q", nf.caFinger, nf.token)
	}
}

// TestNetFlagsResolveNamedContext: --context selects a non-active context.
func TestNetFlagsResolveNamedContext(t *testing.T) {
	withCleanConfig(t)
	if err := deployLogin("ci", "https://ci-vm:9443", "ci-tok", "ci-fp", ""); err != nil {
		t.Fatalf("deployLogin(ci): %v", err)
	}

	nf := &netFlags{context: "ci"}
	nf.applyContextDefaults()
	if nf.server != "https://ci-vm:9443" || nf.token != "ci-tok" || nf.caFinger != "ci-fp" {
		t.Fatalf("named context not resolved: %+v", nf)
	}
}

// TestNetFlagsNoContextStaysLocal: with nothing selected, ResolveContext errors
// and applyContextDefaults leaves the flags untouched — the caller then takes
// the local socket only because no --server was ever requested. What must NOT
// happen is silently inheriting some other context's server.
func TestNetFlagsNoContextStaysLocal(t *testing.T) {
	withCleanConfig(t)
	nf := &netFlags{}
	nf.applyContextDefaults()
	if nf.server != "" || nf.token != "" || nf.caFinger != "" {
		t.Fatalf("flags filled from nowhere: %+v", nf)
	}
	if nf.remote() {
		t.Fatal("remote() should be false with no context and no --server")
	}
}
