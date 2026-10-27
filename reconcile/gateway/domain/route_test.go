package domain

import "testing"

func TestNewRouteLowercasesHost(t *testing.T) {
	r, err := NewRoute("node-1", "production", "api", "API.Example.COM", "", 0, 0)
	if err != nil {
		t.Fatalf("NewRoute: %v", err)
	}
	if r.Host != "api.example.com" {
		t.Fatalf("Host = %q, want it lower-cased", r.Host)
	}
}

func TestNewRouteRejectsHostWithPort(t *testing.T) {
	if _, err := NewRoute("node-1", "production", "api", "api.example.com:8080", "", 0, 0); err == nil {
		t.Fatal("expected a host carrying a port to be rejected")
	}
}

func TestNewRouteRejectsHostWithPath(t *testing.T) {
	if _, err := NewRoute("node-1", "production", "api", "api.example.com/v1", "", 0, 0); err == nil {
		t.Fatal("expected a host carrying a path to be rejected")
	}
}

func TestNewRouteRejectsRelativePath(t *testing.T) {
	if _, err := NewRoute("node-1", "production", "api", "api.example.com", "v1", 0, 0); err == nil {
		t.Fatal("expected a path without a leading slash to be rejected")
	}
}

func TestNewRouteAcceptsEmptyPathAsHostRoot(t *testing.T) {
	r, err := NewRoute("node-1", "production", "api", "api.example.com", "", 0, 0)
	if err != nil {
		t.Fatalf("NewRoute: %v", err)
	}
	if r.Path != "" {
		t.Fatalf("Path = %q, want empty for the host root", r.Path)
	}
}

func TestNewRouteRejectsNegativeRateAndBurst(t *testing.T) {
	if _, err := NewRoute("node-1", "production", "api", "api.example.com", "", -1, 0); err == nil {
		t.Fatal("expected a negative rate to be rejected")
	}
	if _, err := NewRoute("node-1", "production", "api", "api.example.com", "", 0, -1); err == nil {
		t.Fatal("expected a negative burst to be rejected")
	}
}

func TestNewRouteRequiresServiceAndSpace(t *testing.T) {
	if _, err := NewRoute("node-1", "", "api", "api.example.com", "", 0, 0); err == nil {
		t.Fatal("expected a missing space to be rejected")
	}
	if _, err := NewRoute("node-1", "production", " ", "api.example.com", "", 0, 0); err == nil {
		t.Fatal("expected a blank service to be rejected")
	}
}

func TestUpdateKeepsIDAndNode(t *testing.T) {
	r, err := NewRoute("node-1", "production", "api", "api.example.com", "", 0, 0)
	if err != nil {
		t.Fatalf("NewRoute: %v", err)
	}
	id := r.ID
	if err := r.Update("staging", "web", "WEB.example.com", "/app", 10, 5); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if r.ID != id || r.NodeID != "node-1" {
		t.Fatalf("Update moved identity: id=%q node=%q", r.ID, r.NodeID)
	}
	if r.Host != "web.example.com" || r.Path != "/app" || r.Service != "web" {
		t.Fatalf("Update did not apply: %+v", r)
	}
}

func TestUpdateRejectsInvalidWithoutMutating(t *testing.T) {
	r, err := NewRoute("node-1", "production", "api", "api.example.com", "", 0, 0)
	if err != nil {
		t.Fatalf("NewRoute: %v", err)
	}
	if err := r.Update("production", "api", "bad host:9000", "", 0, 0); err == nil {
		t.Fatal("expected an invalid host to be rejected")
	}
	if r.Host != "api.example.com" {
		t.Fatalf("Host = %q, want the route left untouched by a failed update", r.Host)
	}
}
