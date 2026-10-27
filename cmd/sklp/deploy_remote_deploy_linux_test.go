package main

import (
	"os"
	"testing"
)

// TestParseDeployTransport_LocalWhenNoServer: a bare deploy with no context and
// no flags resolves to a local transport (server empty), so the plain-deploy
// router falls through to the socket flow.
func TestParseDeployTransport_LocalWhenNoServer(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // isolate from the real config
	t.Setenv("SKLP_PROFILE", "")
	t.Setenv("SKLP_TOKEN", "")

	dp, err := parseDeployTransport([]string{"core", "--space", "staging"})
	if err != nil {
		t.Fatal(err)
	}
	if dp.server != "" {
		t.Fatalf("expected local (empty server), got %q", dp.server)
	}
	if dp.stack != "core" {
		t.Fatalf("stack = %q, want core", dp.stack)
	}
	if dp.space != "staging" {
		t.Fatalf("space = %q, want staging", dp.space)
	}
}

// TestParseDeployTransport_ServerFlagWins: an explicit --server makes the
// transport remote even without a configured context.
func TestParseDeployTransport_ServerFlagWins(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dp, err := parseDeployTransport([]string{"core", "--server", "https://vm:9443", "--token", "t", "--ca-fingerprint", "fp"})
	if err != nil {
		t.Fatal(err)
	}
	if dp.server != "https://vm:9443" {
		t.Fatalf("server = %q", dp.server)
	}
	if dp.token != "t" || dp.caFinger != "fp" {
		t.Fatalf("token/finger not parsed: %q / %q", dp.token, dp.caFinger)
	}
}

// TestParseDeployTransport_SpaceDefaults: with no --space and no context space,
// the default matches the deploy verb's default ("production").
func TestParseDeployTransport_SpaceDefaults(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dp, err := parseDeployTransport([]string{"core"})
	if err != nil {
		t.Fatal(err)
	}
	if dp.space != "production" {
		t.Fatalf("space default = %q, want production", dp.space)
	}
}

// TestMaybeRunRemoteDeploy_SubverbNotClaimed: a sub-verb invocation is left for
// the other routers (handled=false).
func TestMaybeRunRemoteDeploy_SubverbNotClaimed(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, v := range []string{"ls", "logs", "exec", "delete", "context", "login"} {
		if handled, _ := maybeRunRemoteDeploy([]string{v, "api"}); handled {
			t.Fatalf("sub-verb %q should not be claimed by the plain-deploy router", v)
		}
	}
}

// TestMaybeRunRemoteDeploy_LocalFallsThrough: a local context (no server) is not
// claimed, so the normal local deploy flow runs.
func TestMaybeRunRemoteDeploy_LocalFallsThrough(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("SKLP_TOKEN", "")
	if handled, _ := maybeRunRemoteDeploy([]string{"core"}); handled {
		t.Fatal("local context deploy should fall through (handled=false)")
	}
}

// TestMaybeRunRemoteDeploy_LocalFlagForcesSocket: --local forces the socket flow
// even if a --server is on the line.
func TestMaybeRunRemoteDeploy_LocalFlagForcesSocket(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if handled, _ := maybeRunRemoteDeploy([]string{"core", "--local", "--server", "https://vm:9443"}); handled {
		t.Fatal("--local must force the socket flow (handled=false)")
	}
}

// TestParseDeployTransport_ContextFlagWinsOverActive: --context targets that
// context's node, not the active one, and the resolved name is kept so the
// apply can say where it is going.
func TestParseDeployTransport_ContextFlagWinsOverActive(t *testing.T) {
	dir := t.TempDir()
	cfg := dir + "/config.yaml"
	if err := os.WriteFile(cfg, []byte(`current_context: nats
contexts:
    front:
        server: https://front:6464
        token: tf
        space: meshauthority
    nats:
        server: https://nats:6464
        token: tn
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SKLP_CONFIG", cfg)
	t.Setenv("SKLP_CONTEXT", "")

	dp, err := parseDeployTransport([]string{"meshauthority", "-context", "front"})
	if err != nil {
		t.Fatal(err)
	}
	if dp.context != "front" || dp.server != "https://front:6464" || dp.token != "tf" || dp.space != "meshauthority" {
		t.Fatalf("resolved %+v, want the front context", dp)
	}
	if got := describeTarget(dp); got != `context "front" → https://front:6464` {
		t.Fatalf("describeTarget = %q", got)
	}

	dp, err = parseDeployTransport([]string{"meshauthority"})
	if err != nil {
		t.Fatal(err)
	}
	if dp.context != "nats" || dp.server != "https://nats:6464" || dp.space != "production" {
		t.Fatalf("resolved %+v, want the active nats context", dp)
	}
	if dp.explicitTarget {
		t.Fatal("a target taken from current_context must not count as explicit")
	}

	t.Setenv("SKLP_CONTEXT", "front")
	if dp, _ = parseDeployTransport([]string{"meshauthority"}); !dp.explicitTarget || dp.context != "front" {
		t.Fatalf("SKLP_CONTEXT must count as explicit, got %+v", dp)
	}
}
