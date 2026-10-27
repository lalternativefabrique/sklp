package control

// tls.go manages the GLOBAL self-signed TLS material for the NETWORK listener.
// sklp is its own CA-of-one: it generates an ECDSA P-256 key + a self-signed
// leaf the first time `--listen` is used, persists them 0600 under the global
// control dir, and reuses them afterwards (a reboot keeps the same cert, so a
// client's pinned fingerprint stays valid). The identity is shared across every
// space so a space teardown never rotates the fingerprint. No external PKI, no
// dependency beyond crypto/* + encoding/pem (stdlib).

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// certValidity is the self-signed cert lifetime — long (~10 years) because it is
// a pinned, single-host cert with no rotation story in v1; regenerating it would
// invalidate every client's pin.
const certValidity = 10 * 365 * 24 * time.Hour

// ServerCertPath / ServerKeyPath are the GLOBAL cert/key file paths under the
// control dir. The space arg is ignored (kept for call-site compatibility); the
// identity is shared host-wide. Honour $XDG_CONFIG_HOME via controlConfigDir. Pure.
func ServerCertPath(space string) string {
	return filepath.Join(controlConfigDir(), "server.crt")
}

func ServerKeyPath(space string) string {
	return filepath.Join(controlConfigDir(), "server.key")
}

// EnsureServerCert returns the host's TLS server certificate plus its SHA-256
// fingerprint (the value a remote client pins via --ca-fingerprint). It reuses
// the global stored pair when present; otherwise it PROMOTES a legacy per-space
// cert/key (so an already-deployed VM keeps the same fingerprint) or, failing
// that, generates and persists a fresh self-signed ECDSA P-256 pair (0600).
// Idempotent: the fingerprint is stable across calls once established.
func EnsureServerCert(space string) (tls.Certificate, string, error) {
	certPath, keyPath := ServerCertPath(space), ServerKeyPath(space)

	if cert, fp, err := loadServerCert(certPath, keyPath); err == nil {
		return cert, fp, nil
	} else if !os.IsNotExist(err) {
		return tls.Certificate{}, "", err
	}

	// Migration: promote a legacy per-space cert/key to the global path so a
	// VM deployed before this change keeps its already-pinned fingerprint.
	legacyCert := filepath.Join(spaceConfigDir(space), "server.crt")
	legacyKey := filepath.Join(spaceConfigDir(space), "server.key")
	if cert, fp, err := loadServerCert(legacyCert, legacyKey); err == nil {
		certPEM, kerr := os.ReadFile(legacyCert)
		keyPEM, kerr2 := os.ReadFile(legacyKey)
		if kerr == nil && kerr2 == nil {
			if perr := persistCert(certPath, keyPath, certPEM, keyPEM); perr != nil {
				return tls.Certificate{}, "", perr
			}
			return cert, fp, nil
		}
	} else if !os.IsNotExist(err) {
		return tls.Certificate{}, "", err
	}

	certPEM, keyPEM, err := generateSelfSigned()
	if err != nil {
		return tls.Certificate{}, "", err
	}
	if err := persistCert(certPath, keyPath, certPEM, keyPEM); err != nil {
		return tls.Certificate{}, "", err
	}
	return parseCert(certPEM, keyPEM)
}

// loadServerCert reads a persisted cert+key pair and returns the tls.Certificate
// and its fingerprint. A missing cert/key surfaces os.IsNotExist so EnsureServerCert
// falls through to generation.
func loadServerCert(certPath, keyPath string) (tls.Certificate, string, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	return parseCert(certPEM, keyPEM)
}

// parseCert builds a tls.Certificate from PEM and computes the leaf fingerprint.
func parseCert(certPEM, keyPEM []byte) (tls.Certificate, string, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("parse server cert: %w", err)
	}
	if len(cert.Certificate) == 0 {
		return tls.Certificate{}, "", fmt.Errorf("server cert has no certificate block")
	}
	sum := sha256.Sum256(cert.Certificate[0])
	return cert, hex.EncodeToString(sum[:]), nil
}

// generateSelfSigned creates a fresh ECDSA P-256 self-signed leaf valid for
// certValidity, with the local IPs + hostname as SANs so a client connecting by
// any of them passes hostname verification (when not pinning by fingerprint).
func generateSelfSigned() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate serial: %w", err)
	}

	hostnames, ips := localSANs()
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "sklp-deploy"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              hostnames,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal key: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// localSANs collects the SANs to embed: localhost + the resolved hostname, and
// loopback + every non-loopback unicast IP of the host. Best-effort — a lookup
// failure simply yields fewer SANs (a fingerprint pin does not need any).
func localSANs() (dnsNames []string, ips []net.IP) {
	dnsNames = []string{"localhost"}
	if h, err := os.Hostname(); err == nil && h != "" {
		dnsNames = append(dnsNames, h)
	}
	ips = []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() {
				ips = append(ips, ipn.IP)
			}
		}
	}
	return dnsNames, ips
}

// persistCert writes cert (0644) + key (0600) atomically (temp + rename) under
// the control dir (created 0700). The key is owner-only; the cert is public.
func persistCert(certPath, keyPath string, certPEM, keyPEM []byte) error {
	if err := os.MkdirAll(filepath.Dir(certPath), 0o700); err != nil {
		return fmt.Errorf("create control config dir: %w", err)
	}
	if err := atomicWrite(certPath, certPEM, 0o644); err != nil {
		return fmt.Errorf("write server cert: %w", err)
	}
	if err := atomicWrite(keyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("write server key: %w", err)
	}
	return nil
}

// atomicWrite writes data to a temp file in the target dir, chmods it, then
// renames it into place — so a reader never sees a half-written file.
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
