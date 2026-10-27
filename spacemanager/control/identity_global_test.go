package control

import (
	"os"
	"path/filepath"
	"testing"
)

// TestServerCertGlobalAcrossSpaces: the cert path + fingerprint are identical
// regardless of the space arg (the identity is host-wide, not per-space).
func TestServerCertGlobalAcrossSpaces(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if got, want := ServerCertPath("production"), ServerCertPath("staging"); got != want {
		t.Fatalf("cert path differs by space: %q != %q", got, want)
	}
	_, fpProd, err := EnsureServerCert("production")
	if err != nil {
		t.Fatalf("EnsureServerCert(production): %v", err)
	}
	_, fpStaging, err := EnsureServerCert("staging")
	if err != nil {
		t.Fatalf("EnsureServerCert(staging): %v", err)
	}
	if fpProd != fpStaging {
		t.Fatalf("fingerprint not global: %q != %q", fpProd, fpStaging)
	}
	if dir := filepath.Dir(ServerCertPath("production")); filepath.Base(dir) != "control" {
		t.Fatalf("cert not under control dir: %s", dir)
	}
}

// TestTokenGlobalAcrossSpaces: the token is shared host-wide.
func TestTokenGlobalAcrossSpaces(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	tokProd, err := EnsureToken("production")
	if err != nil {
		t.Fatalf("EnsureToken(production): %v", err)
	}
	tokStaging, err := EnsureToken("staging")
	if err != nil {
		t.Fatalf("EnsureToken(staging): %v", err)
	}
	if tokProd != tokStaging {
		t.Fatalf("token not global: %q != %q", tokProd, tokStaging)
	}
}

// TestEnsureServerCertPromotesLegacy: when only a legacy per-space cert exists,
// EnsureServerCert promotes it to the global path and keeps the SAME fingerprint
// (no break for an already-deployed VM).
func TestEnsureServerCertPromotesLegacy(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// Seed a legacy per-space cert/key by generating one and writing it under
	// the OLD spaces/<space>/ layout.
	certPEM, keyPEM, err := generateSelfSigned()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	legacyDir := spaceConfigDir("production")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatalf("mkdir legacy: %v", err)
	}
	legacyCert := filepath.Join(legacyDir, "server.crt")
	legacyKey := filepath.Join(legacyDir, "server.key")
	if err := os.WriteFile(legacyCert, certPEM, 0o644); err != nil {
		t.Fatalf("write legacy cert: %v", err)
	}
	if err := os.WriteFile(legacyKey, keyPEM, 0o600); err != nil {
		t.Fatalf("write legacy key: %v", err)
	}
	_, wantFP, err := parseCert(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parse legacy: %v", err)
	}

	_, gotFP, err := EnsureServerCert("production")
	if err != nil {
		t.Fatalf("EnsureServerCert: %v", err)
	}
	if gotFP != wantFP {
		t.Fatalf("promotion changed fingerprint: got %q want %q", gotFP, wantFP)
	}
	if _, err := os.Stat(ServerCertPath("production")); err != nil {
		t.Fatalf("global cert not written: %v", err)
	}
}

// TestLegacyPromotionBeforeTeardown encodes the ORDERING contract behind the
// real-VM fix: a legacy spaces/<space>/server.crt exists with NO global cert
// (an already-deployed VM, pre-change). EnsureServerCert MUST promote it to
// control/ on first use — and once promoted, a subsequent RemoveSpaceArtifacts
// (which deletes spaces/<space>/, taking the legacy cert with it) leaves the
// global cert intact with the SAME fingerprint. This is exactly why the deploy
// flow now ensures the identity BEFORE the supervisor/teardown rather than after.
func TestLegacyPromotionBeforeTeardown(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// Seed only a legacy per-space cert/key (no global control/ dir yet).
	certPEM, keyPEM, err := generateSelfSigned()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	legacyDir := spaceConfigDir("production")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatalf("mkdir legacy: %v", err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "server.crt"), certPEM, 0o644); err != nil {
		t.Fatalf("write legacy cert: %v", err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "server.key"), keyPEM, 0o600); err != nil {
		t.Fatalf("write legacy key: %v", err)
	}
	_, wantFP, err := parseCert(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parse legacy: %v", err)
	}
	if _, err := os.Stat(ServerCertPath("production")); !os.IsNotExist(err) {
		t.Fatalf("precondition: global cert must be absent, got err=%v", err)
	}

	// First ensure: promotes the legacy cert to the global path.
	_, gotFP, err := EnsureServerCert("production")
	if err != nil {
		t.Fatalf("EnsureServerCert: %v", err)
	}
	if gotFP != wantFP {
		t.Fatalf("promotion changed fingerprint: got %q want %q", gotFP, wantFP)
	}

	// Now `down` removes the legacy spaces/<space>/ dir (and the legacy cert
	// with it). Because promotion already happened, the global cert survives.
	if err := RemoveSpaceArtifacts("production"); err != nil {
		t.Fatalf("RemoveSpaceArtifacts: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacyDir, "server.crt")); !os.IsNotExist(err) {
		t.Fatalf("legacy cert should be gone after teardown, got err=%v", err)
	}
	_, fpAfter, err := EnsureServerCert("production")
	if err != nil {
		t.Fatalf("EnsureServerCert (post-down): %v", err)
	}
	if fpAfter != wantFP {
		t.Fatalf("fingerprint changed after legacy teardown: got %q want %q", fpAfter, wantFP)
	}
}

// TestEnsureTokenPromotesLegacy: a legacy per-space token is promoted to the
// global path so the VM keeps the SAME token.
func TestEnsureTokenPromotesLegacy(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	legacyDir := spaceConfigDir("production")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatalf("mkdir legacy: %v", err)
	}
	const want = "legacy-token-value"
	if err := os.WriteFile(filepath.Join(legacyDir, "token"), []byte(want+"\n"), 0o600); err != nil {
		t.Fatalf("write legacy token: %v", err)
	}

	got, err := EnsureToken("production")
	if err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}
	if got != want {
		t.Fatalf("promotion changed token: got %q want %q", got, want)
	}
	if _, err := os.Stat(TokenPath("production")); err != nil {
		t.Fatalf("global token not written: %v", err)
	}
}

// TestFingerprintStableAcrossDownRedeploy reproduces the real-VM failure: a
// `--listen` deploy mints the global identity, then `down` runs
// RemoveSpaceArtifacts, then a second `--listen` deploy must reuse the SAME
// fingerprint (and token). The regression was that the identity was promoted
// too late (inside the detached supervisor), AFTER a prior down had already
// deleted the legacy spaces/<space>/ cert — so the redeploy minted a fresh one.
// This asserts the persisted control/server.crt survives the teardown and the
// fingerprint never changes across the down+redeploy cycle.
func TestFingerprintStableAcrossDownRedeploy(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// First `deploy --listen`: ensure the identity (this is what up_embedded.go
	// now does eagerly at deploy time, before any supervisor/teardown).
	_, fp1, err := EnsureServerCert("production")
	if err != nil {
		t.Fatalf("EnsureServerCert (deploy #1): %v", err)
	}
	tok1, err := EnsureToken("production")
	if err != nil {
		t.Fatalf("EnsureToken (deploy #1): %v", err)
	}
	if _, err := os.Stat(ServerCertPath("production")); err != nil {
		t.Fatalf("global cert not persisted after deploy #1: %v", err)
	}

	// Simulate per-space runtime state, then `down` (RemoveSpaceArtifacts).
	if err := os.MkdirAll(spaceConfigDir("production"), 0o700); err != nil {
		t.Fatalf("mkdir space: %v", err)
	}
	if err := RemoveSpaceArtifacts("production"); err != nil {
		t.Fatalf("RemoveSpaceArtifacts: %v", err)
	}

	// The global cert MUST persist through the teardown.
	if _, err := os.Stat(ServerCertPath("production")); err != nil {
		t.Fatalf("control/server.crt removed by teardown: %v", err)
	}

	// Second `deploy --listen`: the identity must be unchanged.
	_, fp2, err := EnsureServerCert("production")
	if err != nil {
		t.Fatalf("EnsureServerCert (deploy #2): %v", err)
	}
	tok2, err := EnsureToken("production")
	if err != nil {
		t.Fatalf("EnsureToken (deploy #2): %v", err)
	}
	if fp1 != fp2 {
		t.Fatalf("fingerprint changed across down+redeploy: %q != %q", fp1, fp2)
	}
	if tok1 != tok2 {
		t.Fatalf("token changed across down+redeploy: %q != %q", tok1, tok2)
	}
}

// TestRemoveSpaceArtifactsPreservesIdentity: tearing down a space must NOT
// remove the global cert/key/token — a client's pin survives.
func TestRemoveSpaceArtifactsPreservesIdentity(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	_, fp1, err := EnsureServerCert("production")
	if err != nil {
		t.Fatalf("EnsureServerCert: %v", err)
	}
	tok1, err := EnsureToken("production")
	if err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}
	// Create some per-space runtime state so the space dir exists.
	if err := os.MkdirAll(spaceConfigDir("production"), 0o700); err != nil {
		t.Fatalf("mkdir space: %v", err)
	}

	if err := RemoveSpaceArtifacts("production"); err != nil {
		t.Fatalf("RemoveSpaceArtifacts: %v", err)
	}

	if _, err := os.Stat(ServerCertPath("production")); err != nil {
		t.Fatalf("cert removed by teardown: %v", err)
	}
	_, fp2, err := EnsureServerCert("production")
	if err != nil {
		t.Fatalf("EnsureServerCert (post-down): %v", err)
	}
	if fp1 != fp2 {
		t.Fatalf("fingerprint changed after down: %q != %q", fp1, fp2)
	}
	tok2, err := LoadToken("production")
	if err != nil {
		t.Fatalf("LoadToken (post-down): %v", err)
	}
	if tok1 != tok2 {
		t.Fatalf("token changed after down: %q != %q", tok1, tok2)
	}
}
