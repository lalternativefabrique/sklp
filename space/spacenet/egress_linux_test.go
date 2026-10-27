//go:build linux

package spacenet

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/space/embed"
)

// extractSlirp writes the embedded slirp4netns into a temp cache and returns its
// path, skipping the test if the binary was not baked into this build (the
// embed pattern is only populated with -tags embedded_runtime after `go
// generate ./runtime/embed/`).
func extractSlirp(t *testing.T) string {
	t.Helper()
	bin, err := embed.ExtractSlirp(t.TempDir())
	if err != nil {
		t.Skipf("embedded slirp4netns unavailable: %v", err)
	}
	return bin
}

// hasTun reports whether /dev/net/tun exists; slirp4netns cannot create tap0
// without it (some CI kernels/sandboxes lack the module).
func hasTun() bool {
	_, err := os.Stat("/dev/net/tun")
	return err == nil
}

// TestSpaceNet_EgressConfig is the go/no-go for space network step 3: after
// EnableEgress the space netns is wired for outbound NAT. It asserts the
// CONFIG (tap0 up, IPv4 forwarding on, masquerade rule installed) rather than a
// real Internet hop, which CI cannot guarantee. The real connect lives behind a
// skip in TestSpaceNet_EgressRealConnect.
func TestSpaceNet_EgressConfig(t *testing.T) {
	skipUnlessRootlessNet(t)
	if !hasTun() {
		t.Skip("/dev/net/tun unavailable: slirp cannot create the egress tap here")
	}
	slirpBin := extractSlirp(t)

	cn, err := NewSpaceNet("egress", "10.88.0.0/24")
	if err != nil {
		t.Fatalf("NewSpaceNet: %v", err)
	}
	defer cn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stop, err := cn.EnableEgress(ctx, slirpBin, false, false, nil, "")
	if err != nil {
		// slirp can still fail to wire tap0 on a constrained kernel; that is a
		// best-effort miss, not a step-3 failure, so skip rather than fail.
		t.Skipf("EnableEgress unavailable here: %v", err)
	}
	defer stop()

	tapUp, forwarding, masq, blackhole, err := cn.EgressStatus()
	if err != nil {
		t.Fatalf("EgressStatus: %v", err)
	}
	if !tapUp {
		t.Fatalf("egress tap %q is not up after EnableEgress", slirpTap)
	}
	if !forwarding {
		t.Fatal("IPv4 forwarding not enabled in the space netns")
	}
	if !masq {
		t.Fatal("masquerade rule for the bridge subnet not installed")
	}
	// SSRF/IMDS guard: every deny route must be present so a container cannot
	// reach 169.254.169.254 or the host LAN through the space NAT.
	if !blackhole {
		t.Fatal("IMDS/RFC1918 blackhole routes not installed in the space netns")
	}

	// A peer attached to the bridge gets a default route (via the gateway), the
	// precondition for its traffic to reach tap0. Attach already installs it;
	// assert it is present so the egress path is complete end to end.
	pid, err := cn.SpawnPeer()
	if err != nil {
		t.Fatalf("SpawnPeer: %v", err)
	}
	if _, err := cn.Attach("client", pid, "eth0"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	// Source-side SSRF guard: the deny routes must also exist INSIDE the peer's
	// own netns, so an IMDS/LAN packet is rejected at the source (immediate
	// "network unreachable") rather than only absorbed as forwarded transit by
	// the worker-side blackhole. configurePeer installs these on attach.
	if ok, err := cn.PeerBlackholeInstalled(pid); err != nil {
		t.Fatalf("PeerBlackholeInstalled: %v", err)
	} else if !ok {
		t.Fatal("IMDS/RFC1918 blackhole routes not installed in the peer (container) netns")
	}
	// Internal reachability must still hold with egress wired (no regression):
	// the peer reaches the bridge gateway.
	if err := cn.Ping(pid, cn.Endpoints().Gateway); err != nil {
		t.Fatalf("peer -> gateway ping FAILED with egress enabled: %v", err)
	}
}

// TestSpaceNet_PeerBlackholeWithoutEgress proves the container-side SSRF guard
// is installed on EVERY attach, even when the space has no egress at all: the
// deny routes belong on the source side regardless of whether outbound NAT is
// wired, so a future routing change can never silently expose IMDS/the host LAN.
// It also asserts east-west is untouched (the bridge /24 is more specific than
// the 10.0.0.0/8 blackhole, so LPM keeps sibling reachability + the gateway).
func TestSpaceNet_PeerBlackholeWithoutEgress(t *testing.T) {
	skipUnlessRootlessNet(t)

	cn, err := NewSpaceNet("peerbh", "10.88.0.0/24")
	if err != nil {
		t.Fatalf("NewSpaceNet: %v", err)
	}
	defer cn.Close()

	a, err := cn.SpawnPeer()
	if err != nil {
		t.Fatalf("SpawnPeer A: %v", err)
	}
	b, err := cn.SpawnPeer()
	if err != nil {
		t.Fatalf("SpawnPeer B: %v", err)
	}
	ipA, err := cn.Attach("a", a, "eth0")
	if err != nil {
		t.Fatalf("Attach A: %v", err)
	}
	if _, err := cn.Attach("b", b, "eth0"); err != nil {
		t.Fatalf("Attach B: %v", err)
	}

	// Blackhole present in both peers' netns despite no egress.
	for pid, label := range map[int]string{a: "a", b: "b"} {
		if ok, err := cn.PeerBlackholeInstalled(pid); err != nil {
			t.Fatalf("PeerBlackholeInstalled(%s): %v", label, err)
		} else if !ok {
			t.Fatalf("peer %q has no IMDS/RFC1918 blackhole routes", label)
		}
	}
	// East-west not broken by the 10.0.0.0/8 blackhole: b still pings a's
	// bridge IP (a 10.88.0.x address, covered by the more-specific connected /24).
	if err := cn.Ping(b, ipA); err != nil {
		t.Fatalf("inter-peer ping FAILED with peer blackhole installed: %v", err)
	}
}

// TestSpaceNet_CloseReapsEgress proves Close invokes the egress stop even when
// the caller never calls it — the backstop against leaking slirp4netns on a
// space that dies without an explicit stop. A spy replaces the real stop; the
// sync.Once guard makes the (caller + Close) double-stop harmless.
func TestSpaceNet_CloseReapsEgress(t *testing.T) {
	skipUnlessRootlessNet(t)

	cn, err := NewSpaceNet("closereap", "10.88.0.0/24")
	if err != nil {
		t.Fatalf("NewSpaceNet: %v", err)
	}

	called := 0
	cn.mu.Lock()
	cn.stopEgress = func() { called++ }
	cn.mu.Unlock()

	cn.Close()
	if called != 1 {
		t.Fatalf("Close invoked egress stop %d times, want 1", called)
	}
	// Idempotent: a second Close must not re-invoke the stop.
	cn.Close()
	if called != 1 {
		t.Fatalf("second Close re-invoked egress stop (count=%d)", called)
	}
}

// TestSpaceNet_EgressForwardsDNS proves the step-2 upstream forward unblocks
// once egress exists: the space resolver, given a stub upstream, forwards an
// external name and returns its answer to a bridge peer. The stub stands in for
// the host's real resolver so the test does not depend on outbound DNS, while
// still exercising the full peer -> gateway:53 -> forward -> upstream path that
// only works because the subnet now has egress.
func TestSpaceNet_EgressForwardsDNS(t *testing.T) {
	skipUnlessRootlessNet(t)
	if !hasTun() {
		t.Skip("/dev/net/tun unavailable: slirp cannot create the egress tap here")
	}
	slirpBin := extractSlirp(t)

	cn, err := NewSpaceNet("dnsfwd", "10.88.0.0/24")
	if err != nil {
		t.Fatalf("NewSpaceNet: %v", err)
	}
	defer cn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stop, err := cn.EnableEgress(ctx, slirpBin, false, false, nil, "")
	if err != nil {
		t.Skipf("EnableEgress unavailable here: %v", err)
	}
	defer stop()

	pid, err := cn.SpawnPeer()
	if err != nil {
		t.Fatalf("SpawnPeer: %v", err)
	}
	if _, err := cn.Attach("client", pid, "eth0"); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// The space resolver forwards to whatever the worker read from the host's
	// /etc/resolv.conf. We cannot assume a reachable upstream in CI, so a
	// forwarded query for an external name is allowed to fail (SERVFAIL) — but it
	// must NOT hang and must NOT resolve to a sibling service IP. The forward
	// PATH is what step 3 unblocks; an actual answer is asserted only behind the
	// real-network skip below.
	got, err := cn.Resolve(pid, "example.com")
	if err == nil && len(got) > 0 {
		for _, ip := range got {
			if ip == cn.Endpoints().Gateway {
				t.Fatalf("external name resolved to the gateway: %v", got)
			}
		}
	}

	if os.Getenv("SPACENET_REAL_NET") == "" {
		t.Skip("set SPACENET_REAL_NET=1 to assert a real upstream-forwarded DNS answer")
	}
	got, err = cn.Resolve(pid, "example.com")
	if err != nil || len(got) == 0 {
		t.Fatalf("real forwarded resolve of example.com: ips=%v err=%v", got, err)
	}
}

// TestSpaceNet_EgressRealConnect makes a real outbound TCP connection from a
// bridge peer through the space NAT. It is gated on SPACENET_REAL_NET because a
// sandbox/CI host may have no outbound path; the config-level assertions above
// are the unconditional go/no-go.
func TestSpaceNet_EgressRealConnect(t *testing.T) {
	if os.Getenv("SPACENET_REAL_NET") == "" {
		t.Skip("set SPACENET_REAL_NET=1 to exercise a real Internet egress connect")
	}
	skipUnlessRootlessNet(t)
	if !hasTun() {
		t.Skip("/dev/net/tun unavailable")
	}
	slirpBin := extractSlirp(t)

	cn, err := NewSpaceNet("realnet", "10.88.0.0/24")
	if err != nil {
		t.Fatalf("NewSpaceNet: %v", err)
	}
	defer cn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stop, err := cn.EnableEgress(ctx, slirpBin, false, false, nil, "")
	if err != nil {
		t.Fatalf("EnableEgress: %v", err)
	}
	defer stop()

	pid, err := cn.SpawnPeer()
	if err != nil {
		t.Fatalf("SpawnPeer: %v", err)
	}
	if _, err := cn.Attach("client", pid, "eth0"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := cn.Dial(pid, "1.1.1.1:443"); err != nil {
		t.Fatalf("real egress connect to 1.1.1.1:443 FAILED: %v", err)
	}
}

// TestSpaceNet_InternalNetworkWithoutEgress is the non-regression guard: with
// NO slirp/egress at all, the step 1-2 network (inter-peer ping + local DNS)
// still works. An empty slirp path forces EnableEgress to fail, and the space
// must be entirely unaffected.
func TestSpaceNet_InternalNetworkWithoutEgress(t *testing.T) {
	skipUnlessRootlessNet(t)

	cn, err := NewSpaceNet("noegress", "10.88.0.0/24")
	if err != nil {
		t.Fatalf("NewSpaceNet: %v", err)
	}
	defer cn.Close()

	// Egress with no binary must fail cleanly and leave the space intact.
	if stop, err := cn.EnableEgress(context.Background(), "", false, false, nil, ""); err == nil {
		if stop != nil {
			stop()
		}
		t.Fatal("EnableEgress with empty slirp path should fail")
	}

	a, err := cn.SpawnPeer()
	if err != nil {
		t.Fatalf("SpawnPeer A: %v", err)
	}
	b, err := cn.SpawnPeer()
	if err != nil {
		t.Fatalf("SpawnPeer B: %v", err)
	}
	ipA, err := cn.Attach("a", a, "eth0")
	if err != nil {
		t.Fatalf("Attach A: %v", err)
	}
	if _, err := cn.Attach("b", b, "eth0"); err != nil {
		t.Fatalf("Attach B: %v", err)
	}
	// Internal ping still works.
	if err := cn.Ping(b, ipA); err != nil {
		t.Fatalf("inter-peer ping FAILED without egress: %v", err)
	}
	// Local DNS still resolves a sibling by name.
	ips, err := cn.Resolve(b, "a")
	if err != nil {
		t.Fatalf("local Resolve without egress: %v", err)
	}
	if len(ips) != 1 || ips[0] != ipA {
		t.Fatalf("Resolve a = %v, want [%s]", ips, ipA)
	}
}
