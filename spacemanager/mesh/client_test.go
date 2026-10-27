package mesh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientRegister(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/peers/register" || r.Method != http.MethodPost {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(registerResponse{
			Peer:  Peer{ID: "self", PubKey: "selfpub", PrivateIP: "10.90.0.2", Endpoint: "10.70.0.10:51820", Spaces: []string{"production"}, Services: []ServiceRef{{Space: "production", Service: "api", Port: 8080}}},
			Peers: []Peer{{ID: "self", PubKey: "selfpub", PrivateIP: "10.90.0.2", Spaces: []string{"production"}}, {ID: "peer2", PubKey: "peer2pub", PrivateIP: "10.90.0.3"}},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", testSignKey(t))
	self, peers, err := c.Register(context.Background(), "self", "selfpub", "10.70.0.10:51820", []string{"production"}, []ServiceRef{{Space: "production", Service: "api", Port: 8080}})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("Authorization header = %q", gotAuth)
	}
	if self.PrivateIP != "10.90.0.2" {
		t.Fatalf("self.PrivateIP = %q", self.PrivateIP)
	}
	if len(peers) != 2 {
		t.Fatalf("len(peers) = %d, want 2", len(peers))
	}
	if len(self.Spaces) != 1 || self.Spaces[0] != "production" {
		t.Fatalf("self.Spaces = %v, want [production]", self.Spaces)
	}
	if len(self.Services) != 1 || self.Services[0].Service != "api" || self.Services[0].Port != 8080 {
		t.Fatalf("self.Services = %v, want [{production api 8080}]", self.Services)
	}
}

func TestClientRegisterErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", testSignKey(t))
	if _, _, err := c.Register(context.Background(), "self", "selfpub", "", nil, nil); err == nil {
		t.Fatal("expected error on 401 response")
	}
}

func TestClientHeartbeat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/peers/self/heartbeat" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", testSignKey(t))
	if err := c.Heartbeat(context.Background(), "self"); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
}

func testSignKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

func TestClientDeletePeerIsSignedAndSurfacesTheRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.Header.Get("X-Mesh-Signature") == "" {
			t.Fatalf("unsigned or unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"peer is still alive: only it can remove itself"}`))
	}))
	defer srv.Close()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)

	err := NewClient(srv.URL, "tok", priv).DeletePeer(context.Background(), "node-b")
	if err == nil || !strings.Contains(err.Error(), "still alive") {
		t.Fatalf("DeletePeer error = %v, want the authority's reason", err)
	}
}

func TestVerifyPinnedLeafIgnoresChainCertificates(t *testing.T) {
	pinned := []byte("pinned-cert")
	sum := sha256.Sum256(pinned)
	fp := hex.EncodeToString(sum[:])

	if err := verifyPinnedLeaf([][]byte{pinned}, fp); err != nil {
		t.Fatalf("pinned leaf rejected: %v", err)
	}
	if err := verifyPinnedLeaf([][]byte{[]byte("attacker-leaf"), pinned}, fp); err == nil {
		t.Fatal("pinned certificate in the chain behind a foreign leaf was accepted")
	}
	if err := verifyPinnedLeaf(nil, fp); err == nil {
		t.Fatal("empty chain accepted")
	}
}
