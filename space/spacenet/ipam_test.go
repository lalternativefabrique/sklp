//go:build linux

package spacenet

import "testing"

func TestNewIPAM_GatewayAndFirst(t *testing.T) {
	a, err := newIPAM("10.88.0.0/24")
	if err != nil {
		t.Fatalf("newIPAM: %v", err)
	}
	if got := a.gateway.String(); got != "10.88.0.1" {
		t.Fatalf("gateway = %s, want 10.88.0.1", got)
	}
	if got := a.gatewayCIDR(); got != "10.88.0.1/24" {
		t.Fatalf("gatewayCIDR = %s, want 10.88.0.1/24", got)
	}
	cidr, ip, err := a.allocate()
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if ip.String() != "10.88.0.2" || cidr != "10.88.0.2/24" {
		t.Fatalf("first allocate = %s (%s), want 10.88.0.2/24", ip, cidr)
	}
}

func TestIPAM_SequentialNeverGateway(t *testing.T) {
	a, _ := newIPAM("10.88.0.0/24")
	seen := map[string]bool{}
	for i := 0; i < 10; i++ {
		_, ip, err := a.allocate()
		if err != nil {
			t.Fatalf("allocate %d: %v", i, err)
		}
		s := ip.String()
		if s == "10.88.0.1" {
			t.Fatalf("allocated the gateway address")
		}
		if seen[s] {
			t.Fatalf("duplicate allocation %s", s)
		}
		seen[s] = true
	}
}

func TestIPAM_ReleaseRecycles(t *testing.T) {
	a, _ := newIPAM("10.88.0.0/24")
	_, ip1, _ := a.allocate() // .2
	_, ip2, _ := a.allocate() // .3
	a.release(ip1)
	// Exhaust forward then wrap; .2 must be handed out again eventually.
	found := false
	for i := 0; i < 260; i++ {
		_, ip, err := a.allocate()
		if err != nil {
			break
		}
		if ip.Equal(ip1) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("released %s was never recycled", ip1)
	}
	_ = ip2
}

func TestIPAM_Exhaustion(t *testing.T) {
	// /30: usable hosts are .1(gw) and .2 — exactly one container address.
	a, err := newIPAM("10.0.0.0/30")
	if err != nil {
		t.Fatalf("newIPAM: %v", err)
	}
	if _, _, err := a.allocate(); err != nil {
		t.Fatalf("first /30 allocate failed: %v", err)
	}
	if _, _, err := a.allocate(); err == nil {
		t.Fatalf("expected exhaustion on second /30 allocate")
	}
}

func TestNewIPAM_RejectsBadInput(t *testing.T) {
	for _, bad := range []string{"", "nonsense", "10.0.0.0", "::1/64", "fd00::/8"} {
		if _, err := newIPAM(bad); err == nil {
			t.Fatalf("newIPAM(%q) = nil error, want failure", bad)
		}
	}
}

func TestBridgeName_Bounds(t *testing.T) {
	// Every bridge name must fit IFNAMSIZ-1 (15 bytes) and be stable.
	for _, in := range []string{"web", "My-API_Gateway-007", "", "web-staging-1", "web-staging-2"} {
		got := bridgeName(in)
		if len(got) > 15 {
			t.Errorf("bridgeName(%q) = %q exceeds IFNAMSIZ-1 (15)", in, got)
		}
		if got != bridgeName(in) {
			t.Errorf("bridgeName(%q) not stable", in)
		}
	}
}

// TestBridgeName_NoPrefixCollision is the D4 fix: two long names sharing a
// prefix must NOT map to the same bridge. The old prefix-truncation collapsed
// them; the hash fragment keeps them distinct (else two spaces silently share
// one bridge).
func TestBridgeName_NoPrefixCollision(t *testing.T) {
	a := bridgeName("web-staging-deployment-1")
	b := bridgeName("web-staging-deployment-2")
	if a == b {
		t.Errorf("two distinct names collide on one bridge: %q", a)
	}
}
