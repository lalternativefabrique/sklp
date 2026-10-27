//go:build linux

package control

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

func putRoutes(t *testing.T, url string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url+RouteGatewayRoutes, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", RouteGatewayRoutes, err)
	}
	return resp
}

// TestGatewayRoutesReplacesWholeSet: the handler hands the supervisor exactly
// the routes in the body. It is a convergence like /v1/apply, so what arrives
// is the desired set, not an append to the live one.
func TestGatewayRoutesReplacesWholeSet(t *testing.T) {
	sup := &fakeSupervisor{routesChanged: true}
	srv := httptest.NewServer(newMux("production", sup, func() {}))
	t.Cleanup(srv.Close)

	body, _ := json.Marshal(GatewayRoutesRequest{Routes: []spacemanager.ResolvedRoute{
		{Service: "api", Host: "api.example.com"},
		{Service: "web", Host: "example.com", Path: "/app"},
	}})
	resp := putRoutes(t, srv.URL, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}

	var out GatewayRoutesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Changed || out.Routes != 2 {
		t.Fatalf("got %+v, want Changed=true Routes=2", out)
	}
	if len(sup.routesSet) != 2 || sup.routesSet[0].Host != "api.example.com" || sup.routesSet[1].Path != "/app" {
		t.Fatalf("SetRoutes got %+v, want the 2 posted routes", sup.routesSet)
	}
}

// TestGatewayRoutesEmptyClearsRoutes: an empty set is a legitimate request that
// takes every route down, not a malformed body. This is how the front door is
// emptied.
func TestGatewayRoutesEmptyClearsRoutes(t *testing.T) {
	sup := &fakeSupervisor{routesChanged: true}
	srv := httptest.NewServer(newMux("production", sup, func() {}))
	t.Cleanup(srv.Close)

	resp := putRoutes(t, srv.URL, []byte(`{"routes":[]}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if len(sup.routesSet) != 0 {
		t.Fatalf("SetRoutes got %+v, want an empty set", sup.routesSet)
	}
}

// TestGatewayRoutesUnchangedReportsNoChange: an identical set is a no-op, so a
// caller reconciling on a timer can tell convergence from a real change.
func TestGatewayRoutesUnchangedReportsNoChange(t *testing.T) {
	sup := &fakeSupervisor{routesChanged: false}
	srv := httptest.NewServer(newMux("production", sup, func() {}))
	t.Cleanup(srv.Close)

	body, _ := json.Marshal(GatewayRoutesRequest{Routes: []spacemanager.ResolvedRoute{
		{Service: "api", Host: "api.example.com"},
	}})
	resp := putRoutes(t, srv.URL, body)
	defer resp.Body.Close()

	var out GatewayRoutesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Changed {
		t.Fatal("Changed = true, want false for an identical set")
	}
}

// TestGatewayRoutesRejectsPost: PUT only — a POST would read as an append, and
// this endpoint never appends.
func TestGatewayRoutesRejectsPost(t *testing.T) {
	sup := &fakeSupervisor{}
	srv := httptest.NewServer(newMux("production", sup, func() {}))
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+RouteGatewayRoutes, "application/json", bytes.NewReader([]byte(`{"routes":[]}`)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", resp.StatusCode)
	}
	if sup.routesSet != nil {
		t.Fatalf("SetRoutes called on a rejected method: %+v", sup.routesSet)
	}
}

// TestGatewayRoutesRejectsMalformedBody: a bad body is a 400 and must not touch
// the live route set — a parse failure never empties the front door.
func TestGatewayRoutesRejectsMalformedBody(t *testing.T) {
	sup := &fakeSupervisor{}
	srv := httptest.NewServer(newMux("production", sup, func() {}))
	t.Cleanup(srv.Close)

	resp := putRoutes(t, srv.URL, []byte(`{"routes":`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
	if sup.routesSet != nil {
		t.Fatalf("SetRoutes called on a malformed body: %+v", sup.routesSet)
	}
}

// TestGatewayRoutesRequiresTokenOverNetwork: the route endpoint mutates the
// public front door, so it must sit BEHIND the network listener's token auth
// like every other verb — an unauthenticated caller must not reach SetRoutes.
func TestGatewayRoutesRequiresTokenOverNetwork(t *testing.T) {
	sup := &fakeSupervisor{routesChanged: true}
	srv := httptest.NewServer(tokenAuth(testToken, newMux("production", sup, func() {})))
	t.Cleanup(srv.Close)

	resp := putRoutes(t, srv.URL, []byte(`{"routes":[]}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401 without a bearer token", resp.StatusCode)
	}
	if sup.routesSet != nil {
		t.Fatalf("SetRoutes reached without auth: %+v", sup.routesSet)
	}
}
