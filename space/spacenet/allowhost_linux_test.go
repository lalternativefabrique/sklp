//go:build linux

package spacenet

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
)

// TestHostAllowAddrs_NeverExemptsLinkLocal is the SSRF guard: whatever the host
// interfaces look like, allowHost must never produce a route toward the cloud
// metadata range. A 169.254.0.0/16 exemption would hand a dev workload the IAM
// credentials of the machine.
func TestHostAllowAddrs_NeverExemptsLinkLocal(t *testing.T) {
	addrs, err := hostAllowAddrs()
	if err != nil {
		t.Skipf("host interfaces unavailable: %v", err)
	}
	for _, cidr := range addrs {
		ip, _, perr := net.ParseCIDR(cidr)
		if perr != nil {
			t.Fatalf("hostAllowAddrs returned unparseable %q: %v", cidr, perr)
		}
		if ip.IsLinkLocalUnicast() {
			t.Fatalf("link-local %s exempted: IMDS must stay unreachable", cidr)
		}
		if ip.IsLoopback() {
			t.Fatalf("loopback %s exempted: slirp --disable-host-loopback makes it meaningless", cidr)
		}
	}
}

// TestHostAllowAddrs_OnlySingleHostRoutes asserts every exemption is a /32.
// A wider prefix would re-open a whole LAN segment rather than the host, which
// is the failure mode allowHost exists to avoid.
func TestHostAllowAddrs_OnlySingleHostRoutes(t *testing.T) {
	addrs, err := hostAllowAddrs()
	if err != nil {
		t.Skipf("host interfaces unavailable: %v", err)
	}
	for _, cidr := range addrs {
		_, n, perr := net.ParseCIDR(cidr)
		if perr != nil {
			t.Fatalf("unparseable %q: %v", cidr, perr)
		}
		if ones, bits := n.Mask.Size(); ones != 32 || bits != 32 {
			t.Fatalf("exemption %s is not a /32 (got /%d): allowHost must not open a subnet", cidr, ones)
		}
	}
}

// TestHostAllowInstalled_EmptyIsSatisfied pins the closed-default semantics:
// requesting no exemption is a satisfied state, not a missing one. EgressStatus
// relies on this to report AllowHost=true on the deploy/CI paths.
func TestHostAllowInstalled_EmptyIsSatisfied(t *testing.T) {
	if !hostAllowInstalled(nil) {
		t.Fatal("no requested exemption must report satisfied")
	}
}

// TestSpaceNet_AllowHostReachesHostService is the end-to-end proof: the SAME
// host address is unreachable from the space netns with allowHost=false and
// reachable with allowHost=true. It needs a listener on a host LAN address, so
// it starts one and skips if the host has no such address.
func TestSpaceNet_AllowHostReachesHostService(t *testing.T) {
	skipUnlessRootlessNet(t)
	if !hasTun() {
		t.Skip("/dev/net/tun unavailable: slirp cannot create the egress tap here")
	}
	addrs, err := hostAllowAddrs()
	if err != nil || len(addrs) == 0 {
		t.Skip("host has no non-bridge global IPv4 address")
	}
	hostIP, _, _ := net.ParseCIDR(addrs[0])

	ln, err := net.Listen("tcp", hostIP.String()+":0")
	if err != nil {
		t.Skipf("cannot listen on %s: %v", hostIP, err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			conn.Close()
		}
	}()
	target := ln.Addr().String()

	slirpBin := extractSlirp(t)
	for _, tc := range []struct {
		name      string
		allowHost bool
		want      bool
	}{
		{"denied by the blackhole", false, false},
		{"allowed by the /32 exception", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cn, cerr := NewSpaceNet("allowhost-e2e", "10.88.0.0/24")
			if cerr != nil {
				t.Fatalf("NewSpaceNet: %v", cerr)
			}
			defer cn.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			stop, eerr := cn.EnableEgress(ctx, slirpBin, tc.allowHost, false, nil, "")
			if eerr != nil {
				t.Skipf("EnableEgress unavailable here: %v", eerr)
			}
			defer stop()

			if got := cn.ProbeTCP(target); got != tc.want {
				t.Fatalf("ProbeTCP(%s) with allowHost=%v = %v, want %v",
					target, tc.allowHost, got, tc.want)
			}
		})
	}
}

// TestSpaceNet_AllowHostOff is the non-regression guard for deploy and CI: with
// allowHost=false the blackhole is intact and no host address is routable.
func TestSpaceNet_AllowHostOff(t *testing.T) {
	skipUnlessRootlessNet(t)
	if !hasTun() {
		t.Skip("/dev/net/tun unavailable: slirp cannot create the egress tap here")
	}
	slirpBin := extractSlirp(t)

	cn, err := NewSpaceNet("allowhost-off", "10.88.0.0/24")
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

	st, err := cn.EgressState()
	if err != nil {
		t.Fatalf("EgressState: %v", err)
	}
	if !st.Blackhole {
		t.Fatal("blackhole missing with allowHost=false")
	}
	// The host's own addresses must still be denied on this path.
	addrs, aerr := hostAllowAddrs()
	if aerr == nil && len(addrs) > 0 && hostAllowInstalled(addrs) {
		t.Fatalf("host addresses %v routable with allowHost=false", addrs)
	}
}

// TestSpaceNet_AllowHostOn asserts the exemption is actually in force AND that
// it did not cost the blackhole: both must hold at once, since the whole design
// is "add a more specific route", never "remove a deny route".
func TestSpaceNet_AllowHostOn(t *testing.T) {
	skipUnlessRootlessNet(t)
	if !hasTun() {
		t.Skip("/dev/net/tun unavailable: slirp cannot create the egress tap here")
	}
	slirpBin := extractSlirp(t)

	cn, err := NewSpaceNet("allowhost-on", "10.88.0.0/24")
	if err != nil {
		t.Fatalf("NewSpaceNet: %v", err)
	}
	defer cn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stop, err := cn.EnableEgress(ctx, slirpBin, true, false, nil, "")
	if err != nil {
		t.Skipf("EnableEgress unavailable here: %v", err)
	}
	defer stop()

	st, err := cn.EgressState()
	if err != nil {
		t.Fatalf("EgressState: %v", err)
	}
	if !st.Blackhole {
		t.Fatal("allowHost removed the blackhole: it must only add more specific routes")
	}
	if len(cn.allowHostAddrs) == 0 {
		t.Skip("host has no non-bridge global IPv4 address to exempt")
	}
	if !st.AllowHost {
		t.Fatalf("host addresses %v not routed around the blackhole", cn.allowHostAddrs)
	}
}

// TestDefaultGatewayAllowAddr covers the vz case (a NAT gateway is the developer's
// machine and must be exempted) and the guards it shares with the interface
// scan: no gateway, a link-local one, and a non-default route are all ignored.
func TestDefaultGatewayAllowAddr(t *testing.T) {
	cidr := func(s string) *net.IPNet { _, n, _ := net.ParseCIDR(s); return n }
	for _, tc := range []struct {
		name   string
		routes []netlink.Route
		want   string
	}{
		{"vz NAT gateway", []netlink.Route{{Gw: net.ParseIP("192.168.64.1")}}, "192.168.64.1/32"},
		{"explicit 0.0.0.0/0 dst", []netlink.Route{{Dst: cidr("0.0.0.0/0"), Gw: net.ParseIP("10.0.0.1")}}, "10.0.0.1/32"},
		{"no default route", []netlink.Route{{Dst: cidr("10.88.0.0/24")}}, ""},
		{"non-default route with gateway", []netlink.Route{{Dst: cidr("10.70.0.0/24"), Gw: net.ParseIP("10.70.0.1")}}, ""},
		{"link-local gateway stays denied", []netlink.Route{{Gw: net.ParseIP("169.254.1.1")}}, ""},
		{"on-link default without gateway", []netlink.Route{{LinkIndex: 2}}, ""},
		{"ipv6 gateway ignored", []netlink.Route{{Gw: net.ParseIP("fe80::1")}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := defaultGatewayAllowAddr(tc.routes); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
