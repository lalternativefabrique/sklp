package control

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/ingress"
)

// TestProbeCertReadsTheServedCertificate: the probe reports what the handshake
// yielded, and marks the self-signed fallback a front door serves when ACME
// never issued for a host — the state an operator is looking for.
func TestProbeCertReadsTheServedCertificate(t *testing.T) {
	cert, err := ingress.SelfSignedCert([]string{"probe.test"}, time.Now())
	if err != nil {
		t.Fatalf("SelfSignedCert() = %v", err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatalf("Listen() = %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.(*tls.Conn).Handshake()
			conn.Close()
		}
	}()

	host, port := splitProbeAddr(t, ln.Addr())
	got := probeCertAt(host, port)

	if got.Error != "" {
		t.Fatalf("Error = %q, want none", got.Error)
	}
	if !got.SelfSigned {
		t.Error("SelfSigned = false, want true on a self-signed leaf")
	}
	if got.Issuer != "" {
		t.Errorf("Issuer = %q, want empty on a self-signed leaf", got.Issuer)
	}
	if got.NotAfter.IsZero() {
		t.Error("NotAfter is zero, want the leaf's expiry")
	}
}

// TestProbeCertReportsAnUnreachableHost: a host that does not answer is a
// different state from one served a bad certificate, so it carries an error
// rather than a zero-valued certificate that would read as "no expiry".
func TestProbeCertReportsAnUnreachableHost(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() = %v", err)
	}
	host, port := splitProbeAddr(t, ln.Addr())
	ln.Close()

	got := probeCertAt(host, port)

	if got.Error == "" {
		t.Fatal("Error is empty, want the dial failure")
	}
	if !got.NotAfter.IsZero() || got.SelfSigned {
		t.Errorf("got %+v, want no certificate fields on a failed probe", got)
	}
}

// TestIsSelfSignedRejectsACAIssuedLeaf: a leaf whose issuer is another name is
// not self-signed. Both carry an empty CommonName here, which is the shape the
// fallback cert has — comparing CNs alone would call this one self-signed.
func TestIsSelfSignedRejectsACAIssuedLeaf(t *testing.T) {
	leaf := &x509.Certificate{
		RawSubject: []byte("subject"),
		RawIssuer:  []byte("a different CA"),
	}

	if isSelfSigned(leaf) {
		t.Error("isSelfSigned() = true, want false when the issuer differs")
	}
}

// splitProbeAddr turns a listener address into the host and port the probe
// dials: a test listener owns an ephemeral port, not :443.
func splitProbeAddr(t *testing.T, addr net.Addr) (string, string) {
	t.Helper()
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		t.Fatalf("SplitHostPort(%v) = %v", addr, err)
	}
	return host, port
}
