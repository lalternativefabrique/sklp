package space

import (
	"net"
	"testing"
)

// TestOpenServiceMergesNewMappings reproduces the eaten gateway mapping: a
// service whose declared ingress is already open must still get a listener
// when the gateway later allocates an extra backend port for it.
func TestOpenServiceMergesNewMappings(t *testing.T) {
	h := newHostIngress(&spaceNet{})

	p1 := freePort(t)
	h.OpenService("core", []PortMapping{{HostPort: p1, ContainerPort: 4100}})
	if n := len(h.listeners["core"]); n != 1 {
		t.Fatalf("declared ingress: %d listeners, want 1", n)
	}

	// Same mapping again (second replica Ready): no duplicate bind.
	h.OpenService("core", []PortMapping{{HostPort: p1, ContainerPort: 4100}})
	if n := len(h.listeners["core"]); n != 1 {
		t.Fatalf("repeat call: %d listeners, want 1", n)
	}

	// The gateway allocates a NEW port for the same service: it must bind.
	p2 := freePort(t)
	h.OpenService("core", []PortMapping{{HostPort: p2, ContainerPort: 4100}})
	if n := len(h.listeners["core"]); n != 2 {
		t.Fatalf("gateway mapping: %d listeners, want 2", n)
	}
	// Both ports must actually be bound (Dial would panic the zero-value
	// spaceNet's splice path, so assert on the listener addresses instead).
	got := map[int]bool{}
	for _, ln := range h.listeners["core"] {
		got[ln.Addr().(*net.TCPAddr).Port] = true
	}
	if !got[p1] || !got[p2] {
		t.Fatalf("bound ports %v, want both %d and %d", got, p1, p2)
	}

	h.Close()
}

// TestOpenServiceFailedBindNotRetried keeps the original guarantee: a port
// that failed to bind is not re-attempted in a tight loop.
func TestOpenServiceFailedBindNotRetried(t *testing.T) {
	h := newHostIngress(&spaceNet{})

	// Occupy a port so the bind fails.
	taken, err := net.Listen("tcp", ingressBindHost+":0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	p := taken.Addr().(*net.TCPAddr).Port

	h.OpenService("web", []PortMapping{{HostPort: p, ContainerPort: 80}})
	if n := len(h.listeners["web"]); n != 0 {
		t.Fatalf("failed bind recorded %d listeners, want 0", n)
	}
	if !h.attempted["web"][p] {
		t.Fatal("failed bind not recorded as attempted")
	}
	// Second call with the same port stays a no-op even once the port frees.
	taken.Close()
	h.OpenService("web", []PortMapping{{HostPort: p, ContainerPort: 80}})
	if n := len(h.listeners["web"]); n != 0 {
		t.Fatalf("failed bind was retried: %d listeners, want 0", n)
	}
	h.Close()
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", ingressBindHost+":0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return p
}

// TestOpenServiceUpdatesOpenPort re-opens a bound port with a new mapping: no
// second listener, and new connections follow the latest mapping.
func TestOpenServiceUpdatesOpenPort(t *testing.T) {
	h := newHostIngress(&spaceNet{})
	defer h.Close()

	p := freePort(t)
	h.OpenService("mail", []PortMapping{{HostPort: p, ContainerPort: 25}})
	if h.mapping("mail", p).ProxyProtocol {
		t.Fatal("initial mapping has no proxy flag")
	}

	h.OpenService("mail", []PortMapping{{HostPort: p, ContainerPort: 2525, ProxyProtocol: true}})
	if n := len(h.listeners["mail"]); n != 1 {
		t.Fatalf("%d listeners, want 1", n)
	}
	got := h.mapping("mail", p)
	if !got.ProxyProtocol || got.ContainerPort != 2525 {
		t.Fatalf("mapping = %+v, want container 2525 with proxy", got)
	}
}
