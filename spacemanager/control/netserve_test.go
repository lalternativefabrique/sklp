package control

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

const testToken = "test-secret-token"

// newTestNetServer mounts the SAME auth-wrapped multi-space dispatcher
// ServeNetMulti uses (ServeNet's own implementation, since D3) on an httptest
// TLS server, registered with exactly one space ("production") — exercising
// the single-space fallback path (an unprefixed request, or a client that
// never called WithSpace, still reaches the sole registered space) alongside
// the space-prefixed path (WithSpace("production")) a multi-space-aware
// client uses. Returns the server and its cert fingerprint (for pinning).
func newTestNetServer(t *testing.T, sup Supervisor) (*httptest.Server, string) {
	t.Helper()
	reg := NewRegistry()
	reg.Register("production", sup, func() {})
	handler := tokenAuth(testToken, newMultiMux(reg, nil))
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	cert := srv.Certificate()
	sum := sha256.Sum256(cert.Raw)
	return srv, hex.EncodeToString(sum[:])
}

// TestTokenMiddleware: no token => 401, wrong token => 401, right token => 200.
func TestTokenMiddleware(t *testing.T) {
	sup := &fakeSupervisor{ready: true}
	srv, fp := newTestNetServer(t, sup)

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"wrong token", "Bearer nope", http.StatusUnauthorized},
		{"right token", "Bearer " + testToken, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, srv.URL+RouteHealth, nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			resp, err := pinnedHTTPClient(fp).Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

// pinnedHTTPClient builds an http.Client that trusts the httptest TLS cert for a
// raw probe in the middleware test (the auth, not TLS, is under test here).
func pinnedHTTPClient(_ string) *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true,
	}}}
}

// TestDialNetStateAndApply: the network Client (DialNet, pinned) reaches State and
// Apply through the auth middleware against a fake supervisor — proof the network
// transport reuses the shared mux + Client methods end to end.
func TestDialNetStateAndApply(t *testing.T) {
	sup := &fakeSupervisor{
		ready: true,
		services: []spacemanager.ServiceSnapshot{{
			Name:      "api",
			Instances: []spacemanager.InstanceSnapshot{{ID: "api-0", PID: 7}},
		}},
		differs: map[string]bool{},
		live:    map[string]spacemanager.Service{},
	}
	srv, fp := newTestNetServer(t, sup)

	c, err := DialNet(srv.URL, testToken, WithCAFingerprint(fp))
	if err != nil {
		t.Fatalf("DialNet: %v", err)
	}
	c = c.WithSpace("production")

	st, err := c.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if len(st.Services) != 1 || st.Services[0].Name != "api" {
		t.Fatalf("State = %+v", st)
	}

	spec, err := MarshalApplySpec([]spacemanager.Service{{Name: "api"}}, spacemanager.SpaceMeta{Name: "production"})
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	if _, err := c.Apply(context.Background(), spec); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

// TestDialNetWrongFingerprintFails: a pin that does not match the server cert is
// rejected at the TLS layer (no plaintext fallback).
func TestDialNetWrongFingerprintFails(t *testing.T) {
	sup := &fakeSupervisor{ready: true}
	srv, _ := newTestNetServer(t, sup)

	c, err := DialNet(srv.URL, testToken, WithCAFingerprint(strings.Repeat("00", 32)))
	if err != nil {
		t.Fatalf("DialNet: %v", err)
	}
	if _, err := c.State(context.Background()); err == nil {
		t.Fatal("want fingerprint mismatch error, got nil")
	}
}

// TestDialNetMissingTrustModeRejected: DialNet without a token is refused (no
// anonymous network access).
func TestDialNetMissingToken(t *testing.T) {
	if _, err := DialNet("https://host:9443", ""); err == nil {
		t.Fatal("want error for empty token, got nil")
	}
}

// TestDialNetExecRawAuth: the TTY exec path dials a RAW TLS connection and writes
// its own request line with the Authorization header. Against a service with no
// running instance the server replies 404 BEFORE any takeover — proving the raw
// network path is both authenticated (middleware) and reaches the exec handler.
func TestDialNetExecRawAuth(t *testing.T) {
	sup := &fakeSupervisor{
		ready:       true,
		services:    []spacemanager.ServiceSnapshot{{Name: "api"}},
		execTargets: map[string]spacemanager.ExecTarget{}, // no target => 404
	}
	srv, fp := newTestNetServer(t, sup)

	c, err := DialNet(srv.URL, testToken, WithCAFingerprint(fp))
	if err != nil {
		t.Fatalf("DialNet: %v", err)
	}
	c = c.WithSpace("production")

	// tty=true drives the raw hijack path (execTTY) over TLS.
	_, err = c.Exec(context.Background(), "api", []string{"sh"}, true, 24, 80, strings.NewReader(""), &nopWriter{}, &nopWriter{})
	if err == nil {
		t.Fatal("want error for unresolved exec target, got nil")
	}
	if !strings.Contains(err.Error(), "api") {
		t.Fatalf("error should mention the service: %v", err)
	}
}

// TestDialNetExecRawWrongToken: a raw TTY exec with a bad token is rejected by the
// auth middleware before the handler runs (401 on the raw stream).
func TestDialNetExecRawWrongToken(t *testing.T) {
	sup := &fakeSupervisor{ready: true, services: []spacemanager.ServiceSnapshot{{Name: "api"}}}
	srv, fp := newTestNetServer(t, sup)

	c, err := DialNet(srv.URL, "wrong-token", WithCAFingerprint(fp))
	if err != nil {
		t.Fatalf("DialNet: %v", err)
	}
	c = c.WithSpace("production")
	_, err = c.Exec(context.Background(), "api", []string{"sh"}, true, 24, 80, strings.NewReader(""), &nopWriter{}, &nopWriter{})
	if err == nil {
		t.Fatal("want 401 error for bad token on raw exec, got nil")
	}
}

// nopWriter discards exec output in tests.
type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestNewMuxSharedRoutes: the SAME mux answers the core routes — the proof the
// socket and the network listener serve identical behavior (newMux is shared).
func TestNewMuxSharedRoutes(t *testing.T) {
	sup := &fakeSupervisor{
		ready: true,
		services: []spacemanager.ServiceSnapshot{{
			Name: "api", Instances: []spacemanager.InstanceSnapshot{{ID: "api-0"}},
		}},
	}
	mux := newMux("production", sup, func() {})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for _, route := range []string{RouteHealth, RouteState} {
		resp, err := http.Get(srv.URL + route)
		if err != nil {
			t.Fatalf("GET %s: %v", route, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d", route, resp.StatusCode)
		}
	}
}
