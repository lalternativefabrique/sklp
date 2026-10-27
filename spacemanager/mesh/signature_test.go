package mesh

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// meshauthority builds this same string in its own module; the two cannot
// import each other, so this pins the wire contract. Change one side and this
// fails rather than every mesh write 401-ing in production.
func TestSigningPayloadMatchesTheAuthorityContract(t *testing.T) {
	got := string(signingPayload("POST", "/v1/peers/register", 1700000000, []byte("hello")))
	want := strings.Join([]string{
		"POST",
		"/v1/peers/register",
		"1700000000",
		// sha256("hello")
		"2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
	}, "\n")
	if got != want {
		t.Fatalf("payload drifted:\n got %q\nwant %q", got, want)
	}
}

func TestClientSignsEveryWrite(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)

	var gotKey, gotSig, gotTS string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotSig, gotTS = r.Header.Get("X-Mesh-Sign-Key"), r.Header.Get("X-Mesh-Signature"), r.Header.Get("X-Mesh-Timestamp")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"peer":{"id":"n1"},"peers":[]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", priv)
	if _, _, err := c.Register(t.Context(), "n1", "wgpub", "10.0.0.1:51820", nil, nil); err != nil {
		t.Fatalf("register: %v", err)
	}

	if gotKey != hex.EncodeToString(priv.Public().(ed25519.PublicKey)) {
		t.Fatal("client did not send its signing key")
	}
	ts, err := strconv.ParseInt(gotTS, 10, 64)
	if err != nil {
		t.Fatalf("timestamp header: %v", err)
	}
	sig, err := hex.DecodeString(gotSig)
	if err != nil {
		t.Fatalf("signature header: %v", err)
	}
	body := `{"endpoint":"10.0.0.1:51820","id":"n1","pubkey":"wgpub","services":null,"spaces":null}`
	if !ed25519.Verify(priv.Public().(ed25519.PublicKey), signingPayload("POST", "/v1/peers/register", ts, []byte(body)), sig) {
		t.Fatal("signature does not verify over the body actually sent")
	}
}
