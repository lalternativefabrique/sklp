package space

import (
	"fmt"
	"net"
	"testing"
)

// TestAllocLoopbackPortUsable asserts the allocator returns a real, in-range
// loopback port that is actually bindable right after allocation (the tiny
// TOCTOU window holds in practice on an idle test host).
func TestAllocLoopbackPortUsable(t *testing.T) {
	port, err := allocLoopbackPort()
	if err != nil {
		t.Fatalf("allocLoopbackPort: %v", err)
	}
	if port < 1 || port > 65535 {
		t.Fatalf("port %d out of range", port)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(ingressBindHost, fmt.Sprintf("%d", port)))
	if err != nil {
		t.Fatalf("returned port %d is not bindable: %v", port, err)
	}
	_ = ln.Close()
}

// TestAllocLoopbackPortDistinct asserts two consecutive allocations differ: each
// call asks the kernel for a fresh ephemeral port, so a caller opening several
// gateway ingresses never collides them on one host port.
func TestAllocLoopbackPortDistinct(t *testing.T) {
	a, err := allocLoopbackPort()
	if err != nil {
		t.Fatalf("alloc a: %v", err)
	}
	b, err := allocLoopbackPort()
	if err != nil {
		t.Fatalf("alloc b: %v", err)
	}
	if a == b {
		t.Fatalf("two allocations returned the same port %d", a)
	}
}
