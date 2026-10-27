package control

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNormalizeServer: scheme is inferred, trailing slash dropped, host extracted.
func TestNormalizeServer(t *testing.T) {
	cases := []struct {
		in       string
		baseURL  string
		hostPort string
		wantErr  bool
	}{
		{"https://host:9443", "https://host:9443", "host:9443", false},
		{"host:9443", "https://host:9443", "host:9443", false},
		{"https://host:9443/", "https://host:9443", "host:9443", false},
		{"http://host:9443", "", "", true}, // plain http rejected
		{"", "", "", true},
	}
	for _, tc := range cases {
		base, host, err := normalizeServer(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("normalizeServer(%q): want error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizeServer(%q): %v", tc.in, err)
			continue
		}
		if base != tc.baseURL || host != tc.hostPort {
			t.Errorf("normalizeServer(%q) = (%q,%q), want (%q,%q)", tc.in, base, host, tc.baseURL, tc.hostPort)
		}
	}
}

// TestNormalizeFingerprint: separators and prefixes are stripped, case lowered.
func TestNormalizeFingerprint(t *testing.T) {
	want := "ab12cd34"
	for _, in := range []string{"AB:12:CD:34", "sha256:ab12cd34", "AB12CD34", " ab12cd34 "} {
		if got := normalizeFingerprint(in); got != want {
			t.Errorf("normalizeFingerprint(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBearerRoundTripperInjectsToken: every request through the network client
// carries Authorization: Bearer <token>.
func TestBearerRoundTripperInjectsToken(t *testing.T) {
	gotAuth := make(chan string, 1)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth <- r.Header.Get("Authorization")
		w.Write([]byte(`{"space":"x"}`))
	}))
	defer srv.Close()

	sum := sha256.Sum256(srv.Certificate().Raw)
	fp := hex.EncodeToString(sum[:])

	c, err := DialNet(srv.URL, "tok-123", WithCAFingerprint(fp))
	if err != nil {
		t.Fatalf("DialNet: %v", err)
	}
	if _, err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if auth := <-gotAuth; auth != "Bearer tok-123" {
		t.Fatalf("Authorization = %q, want Bearer tok-123", auth)
	}
}

// TestDialNetInsecureConnects: --insecure skips verification and connects to a
// server whose cert it does not trust.
func TestDialNetInsecureConnects(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"space":"x"}`))
	}))
	defer srv.Close()

	c, err := DialNet(srv.URL, "tok", WithInsecure())
	if err != nil {
		t.Fatalf("DialNet: %v", err)
	}
	if _, err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health insecure: %v", err)
	}
}

// TestClientTLSConfigPinPrecedence: a pin wins over insecure (a pin is stricter).
func TestClientTLSConfigPinPrecedence(t *testing.T) {
	cfg, err := clientTLSConfig(dialConfig{insecure: true, fingerprint: "abc"})
	if err != nil {
		t.Fatalf("clientTLSConfig: %v", err)
	}
	if cfg.VerifyPeerCertificate == nil {
		t.Fatal("pin should install a VerifyPeerCertificate even alongside insecure")
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %x, want TLS12", cfg.MinVersion)
	}
}

// A multi-space node dispatches on SpaceRoutePrefix, so a client scoped to a
// space must put every space-scoped request on that space's routes — the bare
// route would reach whichever supervisor answers the unprefixed path.
func TestWithSpaceScopesEverySpaceRoute(t *testing.T) {
	c, err := DialNet("https://10.0.0.4:6464", "tok", WithInsecure())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c = c.WithSpace("shop-production")
	for _, route := range []string{RouteState, RouteLogs, RouteApply} {
		want := SpaceRoutePrefix("shop-production") + strings.TrimPrefix(route, "/v1")
		if got := c.route(route); got != want {
			t.Errorf("route(%q) = %q, want %q", route, got, want)
		}
	}
	// The inventory is node-scoped: prefixing it would ask a space for the
	// node's whole list.
	if got := c.route(RouteSpaces); strings.HasPrefix(got, SpaceRoutePrefix("shop-production")+"/spaces") {
		t.Errorf("RouteSpaces got prefixed: %q", got)
	}
}

func TestDialNetRawDialSendsSNI(t *testing.T) {
	cert, _ := selfSigned(t, "localhost")
	var seen string
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		GetConfigForClient: func(hi *tls.ClientHelloInfo) (*tls.Config, error) {
			seen = hi.ServerName
			return nil, nil
		},
		Certificates: []tls.Certificate{cert},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		c.(*tls.Conn).Handshake()
		c.Close()
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	c, err := DialNet("https://localhost:"+port, "tok", WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := c.dialRaw(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if seen != "localhost" {
		t.Fatalf("raw dial sent SNI %q, want localhost", seen)
	}
}
