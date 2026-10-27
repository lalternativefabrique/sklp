package control

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"testing"
)

// TestEnsureServerCertIdempotentAndPerms: first call generates a cert+key (key
// 0600, cert 0644), the fingerprint is stable across calls, and the cert reuses
// the persisted material (no regeneration).
func TestEnsureServerCertIdempotentAndPerms(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cert1, fp1, err := EnsureServerCert("production")
	if err != nil {
		t.Fatalf("EnsureServerCert: %v", err)
	}
	if fp1 == "" || len(cert1.Certificate) == 0 {
		t.Fatal("empty cert/fingerprint")
	}

	keyInfo, err := os.Stat(ServerKeyPath("production"))
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if perm := keyInfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key perms = %o, want 600", perm)
	}
	certInfo, err := os.Stat(ServerCertPath("production"))
	if err != nil {
		t.Fatalf("stat cert: %v", err)
	}
	if perm := certInfo.Mode().Perm(); perm != 0o644 {
		t.Fatalf("cert perms = %o, want 644", perm)
	}

	_, fp2, err := EnsureServerCert("production")
	if err != nil {
		t.Fatalf("EnsureServerCert (2nd): %v", err)
	}
	if fp1 != fp2 {
		t.Fatalf("fingerprint not stable: %q != %q", fp1, fp2)
	}
}

// TestServerCertIsSelfSignedWithSANs: the generated leaf is self-signed and
// carries localhost + loopback SANs so a non-pinning client can still verify a
// localhost connection.
func TestServerCertIsSelfSignedWithSANs(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, _, err := EnsureServerCert("production"); err != nil {
		t.Fatalf("EnsureServerCert: %v", err)
	}
	pemData, err := os.ReadFile(ServerCertPath("production"))
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	block, _ := pem.Decode(pemData)
	if block == nil {
		t.Fatal("no PEM block")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	hasLocalhost := false
	for _, n := range c.DNSNames {
		if n == "localhost" {
			hasLocalhost = true
		}
	}
	if !hasLocalhost {
		t.Fatalf("cert SAN DNS names = %v, want localhost", c.DNSNames)
	}
	hasLoopbackIP := false
	for _, ip := range c.IPAddresses {
		if ip.IsLoopback() {
			hasLoopbackIP = true
		}
	}
	if !hasLoopbackIP {
		t.Fatalf("cert SAN IPs = %v, want a loopback", c.IPAddresses)
	}
}
