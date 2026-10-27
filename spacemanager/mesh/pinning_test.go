package mesh

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
)

func tlsAuthority(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	sum := sha256.Sum256(srv.Certificate().Raw)
	return srv, hex.EncodeToString(sum[:])
}

func TestPinnedClientAcceptsTheMatchingCertificate(t *testing.T) {
	srv, fp := tlsAuthority(t)
	defer srv.Close()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)

	c := NewPinnedClient(srv.URL, "tok", priv, fp)
	if _, err := c.ListPeers(t.Context()); err != nil {
		t.Fatalf("pinned client rejected its own authority: %v", err)
	}
}

// Without pinning, a self-signed authority is either unreachable or trusted
// blindly; a wrong pin must fail closed rather than fall back.
func TestPinnedClientRejectsAnotherCertificate(t *testing.T) {
	srv, _ := tlsAuthority(t)
	defer srv.Close()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)

	wrong := hex.EncodeToString(make([]byte, 32))
	c := NewPinnedClient(srv.URL, "tok", priv, wrong)
	if _, err := c.ListPeers(t.Context()); err == nil {
		t.Fatal("client talked to an authority whose certificate it had not pinned")
	}
}

// Operators paste fingerprints straight out of openssl, colons included.
func TestPinAcceptsColonSeparatedFingerprints(t *testing.T) {
	srv, fp := tlsAuthority(t)
	defer srv.Close()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)

	var spaced string
	for i := 0; i < len(fp); i += 2 {
		if i > 0 {
			spaced += ":"
		}
		spaced += fp[i : i+2]
	}
	c := NewPinnedClient(srv.URL, "tok", priv, spaced)
	if _, err := c.ListPeers(t.Context()); err != nil {
		t.Fatalf("colon-separated fingerprint rejected: %v", err)
	}
}
