package control

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

// getJSONFrom GETs one path off a test server and decodes it, for routes the
// typed client has no method for.
func getJSONFrom(t *testing.T, srv *httptest.Server, path string, out any) error {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// newMultiTestServer mounts newMultiMux against reg on an httptest TLS
// server, returning the server and its cert fingerprint (for pinning).
func newMultiTestServer(t *testing.T, reg *Registry) (*httptest.Server, string) {
	t.Helper()
	return newMultiTestServerWithMaterializer(t, reg, nil)
}

// newMultiTestServerWithMaterializer is newMultiTestServer, but also wires a
// Materializer — used by tests exercising on-demand space materialization.
func newMultiTestServerWithMaterializer(t *testing.T, reg *Registry, materialize Materializer) (*httptest.Server, string) {
	t.Helper()
	handler := tokenAuth(testToken, newMultiMux(reg, materialize))
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	cert := srv.Certificate()
	sum := sha256.Sum256(cert.Raw)
	return srv, hex.EncodeToString(sum[:])
}

// TestMultiSpace_TwoSpacesRouteIndependently is the regression guard for the
// whole point of this feature: a node running TWO spaces answers each one at
// its own SpaceRoutePrefix, and a request for one space never reaches the
// other's Supervisor.
func TestMultiSpace_TwoSpacesRouteIndependently(t *testing.T) {
	www := &fakeSupervisor{ready: true, services: []spacemanager.ServiceSnapshot{{Name: "www"}}}
	shop := &fakeSupervisor{ready: true, services: []spacemanager.ServiceSnapshot{{Name: "core"}, {Name: "web"}}}

	reg := NewRegistry()
	reg.Register("www", www, func() {})
	reg.Register("shop", shop, func() {})

	srv, fp := newMultiTestServer(t, reg)

	dial := func(space string) *Client {
		c, err := DialNet(srv.URL, testToken, WithCAFingerprint(fp))
		if err != nil {
			t.Fatalf("DialNet: %v", err)
		}
		return c.WithSpace(space)
	}

	wwwState, err := dial("www").State(context.Background())
	if err != nil {
		t.Fatalf("www State: %v", err)
	}
	if len(wwwState.Services) != 1 || wwwState.Services[0].Name != "www" {
		t.Fatalf("www State = %+v, want the www supervisor's own service list", wwwState)
	}

	shopState, err := dial("shop").State(context.Background())
	if err != nil {
		t.Fatalf("shop State: %v", err)
	}
	if len(shopState.Services) != 2 {
		t.Fatalf("shop State = %+v, want the shop supervisor's own 2 services", shopState)
	}
}

// TestMultiSpace_UnknownSpace404s proves a request naming a space the node
// does not run gets a clean 404, not a match against some other space's
// Supervisor (which would silently serve the wrong node's state).
func TestMultiSpace_UnknownSpace404s(t *testing.T) {
	reg := NewRegistry()
	reg.Register("www", &fakeSupervisor{ready: true}, func() {})
	srv, fp := newMultiTestServer(t, reg)

	c, err := DialNet(srv.URL, testToken, WithCAFingerprint(fp))
	if err != nil {
		t.Fatalf("DialNet: %v", err)
	}
	c = c.WithSpace("nats-global")

	if _, err := c.State(context.Background()); err == nil {
		t.Fatal("expected an error for a space this node does not run, got nil")
	}
}

// TestMultiSpace_UnprefixedRequestAmbiguousWith2Spaces404s proves the
// single-space back-compat fallback (an unprefixed request reaching the sole
// registered space) does NOT kick in once a node runs more than one space —
// an unprefixed request has no unambiguous target then, so it 404s instead
// of guessing.
func TestMultiSpace_UnprefixedRequestAmbiguousWith2Spaces404s(t *testing.T) {
	reg := NewRegistry()
	reg.Register("www", &fakeSupervisor{ready: true}, func() {})
	reg.Register("shop", &fakeSupervisor{ready: true}, func() {})
	srv, fp := newMultiTestServer(t, reg)

	// A client that never calls WithSpace sends bare (unprefixed) routes.
	c, err := DialNet(srv.URL, testToken, WithCAFingerprint(fp))
	if err != nil {
		t.Fatalf("DialNet: %v", err)
	}

	if _, err := c.State(context.Background()); err == nil {
		t.Fatal("expected an unprefixed request against a 2-space node to fail (ambiguous target), got nil")
	}
}

// TestMultiSpace_UnregisterStopsRouting proves a torn-down space (removed
// from the Registry) stops answering — no stale cached mux keeps serving it.
func TestMultiSpace_UnregisterStopsRouting(t *testing.T) {
	reg := NewRegistry()
	reg.Register("www", &fakeSupervisor{ready: true}, func() {})
	srv, fp := newMultiTestServer(t, reg)

	c, err := DialNet(srv.URL, testToken, WithCAFingerprint(fp))
	if err != nil {
		t.Fatalf("DialNet: %v", err)
	}
	c = c.WithSpace("www")

	if _, err := c.State(context.Background()); err != nil {
		t.Fatalf("State before Unregister: %v", err)
	}

	reg.Unregister("www")

	if _, err := c.State(context.Background()); err == nil {
		t.Fatal("expected State to fail after Unregister, got nil")
	}
}

// TestMultiSpace_MaterializesUnknownSpaceOnDemand proves an Apply for a
// space reg has never seen brings it up via materialize instead of 404ing,
// and that a racing second request for the SAME never-yet-seen space is
// deduplicated (materialize runs once). Apply is the only route this should
// happen for — see TestMultiSpace_OtherRoutesDoNotMaterialize for why every
// other verb must 404 instead.
func TestMultiSpace_MaterializesUnknownSpaceOnDemand(t *testing.T) {
	reg := NewRegistry()
	var calls atomic.Int32
	materialize := func(space string) error {
		calls.Add(1)
		reg.Register(space, &fakeSupervisor{ready: true, services: []spacemanager.ServiceSnapshot{{Name: "web"}}}, func() {})
		return nil
	}
	srv, fp := newMultiTestServerWithMaterializer(t, reg, materialize)

	c, err := DialNet(srv.URL, testToken, WithCAFingerprint(fp))
	if err != nil {
		t.Fatalf("DialNet: %v", err)
	}
	c = c.WithSpace("fresh")
	spec, err := MarshalApplySpec([]spacemanager.Service{{Name: "web", Image: "img:1"}}, spacemanager.SpaceMeta{Name: "fresh"})
	if err != nil {
		t.Fatalf("MarshalApplySpec: %v", err)
	}

	const concurrency = 5
	errs := make(chan error, concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			_, serr := c.Apply(context.Background(), spec)
			errs <- serr
		}()
	}
	for i := 0; i < concurrency; i++ {
		if serr := <-errs; serr != nil {
			t.Fatalf("Apply against a materializable space: %v", serr)
		}
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("materialize called %d times, want exactly 1 (concurrent requests should dedupe)", got)
	}
}

// TestMultiSpace_OtherRoutesDoNotMaterialize proves State/Down against a
// space reg has never seen 404 WITHOUT ever calling materialize — only Apply
// (the first-deploy case, sklet/agent.go's AddSpace doc) should
// bring an unknown space up. A materialize on any other route means a typo
// in `sklp deploy delete -space <name>` (or ps/logs/exec/restart) silently
// CREATES an empty space instead of failing loudly — reproduced live on
// node-d, 2026-08-30 (a mistyped delete left stray "nginx" /
// "sapcea" spaces in the cache).
func TestMultiSpace_OtherRoutesDoNotMaterialize(t *testing.T) {
	reg := NewRegistry()
	var calls atomic.Int32
	materialize := func(space string) error {
		calls.Add(1)
		reg.Register(space, &fakeSupervisor{ready: true}, func() {})
		return nil
	}
	srv, fp := newMultiTestServerWithMaterializer(t, reg, materialize)

	c, err := DialNet(srv.URL, testToken, WithCAFingerprint(fp))
	if err != nil {
		t.Fatalf("DialNet: %v", err)
	}
	c = c.WithSpace("typo-space")

	if _, serr := c.State(context.Background()); serr == nil {
		t.Fatal("State against an unregistered space succeeded, want a 404")
	}
	if serr := c.Down(context.Background()); serr == nil {
		t.Fatal("Down against an unregistered space succeeded, want a 404")
	}

	if got := calls.Load(); got != 0 {
		t.Fatalf("materialize called %d times for State/Down, want 0 — a typo must never create a space", got)
	}
	if _, ok := reg.get("typo-space"); ok {
		t.Fatal("typo-space was registered despite materialize never running")
	}
}

// TestMultiSpace_MaterializeFailureIsNot404 proves materialize's error
// surfaces as a real failure (not a bare "unknown space" 404) so a caller can
// distinguish "this space will never exist here" from "it failed to start".
func TestMultiSpace_MaterializeFailureIsNot404(t *testing.T) {
	reg := NewRegistry()
	wantErr := errors.New("no durable data disk configured")
	materialize := func(space string) error { return wantErr }
	srv, fp := newMultiTestServerWithMaterializer(t, reg, materialize)

	c, err := DialNet(srv.URL, testToken, WithCAFingerprint(fp))
	if err != nil {
		t.Fatalf("DialNet: %v", err)
	}
	c = c.WithSpace("broken")

	_, serr := c.State(context.Background())
	if serr == nil {
		t.Fatal("expected an error when materialize fails, got nil")
	}
}

// TestMultiSpace_ManifestServedForStoppedSpace proves the manifest route does
// NOT go through the Registry: a space whose supervisor is gone (stopped, or
// never came up) still answers with its declared shape. That is precisely when
// an operator asks — to find out what the space was supposed to run — so a 404
// there would hide the answer at the moment it matters.
func TestMultiSpace_ManifestServedForStoppedSpace(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := SaveManifest("stopped",
		[]spacemanager.Service{{Name: "web", Image: "rg.example.com/web:1"}},
		spacemanager.SpaceMeta{Name: "stopped"},
	); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}

	// Registry deliberately left empty: nothing supervises "stopped".
	srv, fp := newMultiTestServer(t, NewRegistry())

	client, err := DialNet(srv.URL, testToken, WithCAFingerprint(fp))
	if err != nil {
		t.Fatalf("DialNet: %v", err)
	}
	// The registry is empty, so every supervisor-backed route must still fail.
	if _, err := client.WithSpace("stopped").State(context.Background()); err == nil {
		t.Fatal("State must fail for a space with no supervisor")
	}

	// The space-scoped wire path strips RouteManifest's own "/v1" root: the
	// prefix already carries it (see SpaceRoutePrefix / stripSpacePrefix).
	path := SpaceRoutePrefix("stopped") + strings.TrimPrefix(RouteManifest, "/v1")
	var manifest ManifestResponse
	if err := getJSONFrom(t, srv, path, &manifest); err != nil {
		t.Fatalf("manifest for a stopped space: %v", err)
	}
	if len(manifest.Services) != 1 || manifest.Services[0].Image != "rg.example.com/web:1" {
		t.Fatalf("manifest = %+v", manifest)
	}
}

// A stopped space is absent from the registry — Start must materialise it from
// the manifest it kept, which is the whole point of no longer purging on stop.
func TestStartMaterialisesAStoppedSpace(t *testing.T) {
	reg := NewRegistry()
	var asked string
	materialize := func(space string) error {
		asked = space
		reg.Register(space, &fakeSupervisor{}, func() {})
		return nil
	}
	h := newMultiMux(reg, materialize)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/spaces/alpha/start", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if asked != "alpha" {
		t.Fatalf("materialize called with %q, want alpha", asked)
	}
	if _, ok := reg.get("alpha"); !ok {
		t.Fatal("the space must be registered after a start")
	}
}

// Starting a running space is a no-op, so pressing Start twice is harmless.
func TestStartOnARunningSpaceIsANoop(t *testing.T) {
	reg := NewRegistry()
	reg.Register("alpha", &fakeSupervisor{}, func() {})
	called := false
	h := newMultiMux(reg, func(string) error { called = true; return nil })

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/spaces/alpha/start", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if called {
		t.Fatal("a running space must not be materialised again")
	}
}

// Every other route still 404s for an unknown space: only an explicit start
// (or an apply) may bring one into existence — a typo must not create one.
func TestStateOnAStoppedSpaceStill404s(t *testing.T) {
	reg := NewRegistry()
	h := newMultiMux(reg, func(string) error {
		t.Fatal("state must never materialise a space")
		return nil
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/spaces/typo/state", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
