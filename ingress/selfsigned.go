package ingress

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"sync"
	"time"
)

// selfsigned.go generates an in-memory self-signed certificate the native L7
// uses to TERMINATE TLS. It is the zero-dependency default: HTTPS works
// immediately (curl -k) without ACME or any external CA. A real cert source
// (certmagic / Let's Encrypt, or an operator-provided cert) layers on top later;
// this is what makes TLS testable in dev (*.test, localhost — domains Let's
// Encrypt can never validate).

// SelfSignedCert builds a fresh self-signed ECDSA certificate valid for the
// given hosts (SANs). notBefore is passed in so the result is deterministic in
// tests (time.Now() is unavailable to the spacenet worker's caller). A wildcard
// or bare host both work; an empty hosts list yields a cert with no SAN (still
// usable for `curl -k`). The key never leaves memory.
func SelfSignedCert(hosts []string, notBefore time.Time) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("self-signed: generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("self-signed: serial: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"sklp-ingress"}},
		NotBefore:             notBefore,
		NotAfter:              notBefore.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              hosts,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("self-signed: create cert: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// SelfSignedResolver serves a self-signed certificate PER SNI, generated on
// first request for that host and cached. It is the in-memory tls.Config
// GetCertificate source the native L7 uses for *.test/localhost — the cert's SAN
// matches the requested host, so it validates against that host (no curl -k
// needed to test the hostname binding; the CA is still untrusted, which is the
// point of self-signed). For public domains the operator wires certmagic instead
// (same GetCertificate seam). Safe for concurrent use.
type SelfSignedResolver struct {
	now   func() time.Time // injected for deterministic tests; defaults to time.Now
	mu    sync.Mutex
	certs map[string]*tls.Certificate // SNI host -> cached cert
}

// NewSelfSignedResolver builds an empty per-SNI self-signed resolver.
func NewSelfSignedResolver() *SelfSignedResolver {
	return &SelfSignedResolver{certs: map[string]*tls.Certificate{}}
}

// GetCertificate is the tls.Config.GetCertificate callback: it returns a cached
// self-signed cert for hello.ServerName, minting one (with that host as SAN) on
// first use. An empty SNI falls back to a no-SAN cert (a client that didn't send
// SNI). The error path never blocks the handshake on a transient failure beyond
// surfacing it to crypto/tls.
func (r *SelfSignedResolver) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	host := hello.ServerName
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.certs[host]; ok {
		return c, nil
	}
	var hosts []string
	if host != "" {
		hosts = []string{host}
	}
	now := time.Now
	if r.now != nil {
		now = r.now
	}
	cert, err := SelfSignedCert(hosts, now())
	if err != nil {
		return nil, err
	}
	r.certs[host] = &cert
	return &cert, nil
}
