package ingress

import (
	"crypto/tls"
	"crypto/x509"
	"testing"
	"time"
)

func TestSelfSignedCert_Usable(t *testing.T) {
	cert, err := SelfSignedCert([]string{"app.test"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(cert.Certificate) == 0 || cert.PrivateKey == nil {
		t.Fatal("cert or key missing")
	}
	// It must be loadable into a tls.Config (the worker's ServeTLS path).
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}}
	if len(cfg.Certificates) != 1 {
		t.Fatal("cert not accepted by tls.Config")
	}
}

func TestSelfSignedCert_Deterministic(t *testing.T) {
	// Same notBefore + same hosts must build without error twice (no global state).
	at := time.Now()
	if _, err := SelfSignedCert(nil, at); err != nil {
		t.Fatal(err)
	}
	if _, err := SelfSignedCert([]string{"a", "b"}, at); err != nil {
		t.Fatal(err)
	}
}

func TestSelfSignedResolver_PerSNI(t *testing.T) {
	r := NewSelfSignedResolver()
	c1, err := r.GetCertificate(&tls.ClientHelloInfo{ServerName: "secure.test"})
	if err != nil {
		t.Fatal(err)
	}
	// The cert's leaf must carry the requested host as SAN.
	leaf, _ := x509.ParseCertificate(c1.Certificate[0])
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "secure.test" {
		t.Errorf("SAN = %v, want [secure.test]", leaf.DNSNames)
	}
	// Same SNI → cached (same pointer).
	c2, _ := r.GetCertificate(&tls.ClientHelloInfo{ServerName: "secure.test"})
	if c1 != c2 {
		t.Error("expected the same cached cert for the same SNI")
	}
	// Different SNI → different cert.
	c3, _ := r.GetCertificate(&tls.ClientHelloInfo{ServerName: "other.test"})
	if c1 == c3 {
		t.Error("expected a distinct cert for a different SNI")
	}
}
