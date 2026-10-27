//go:build linux

package spacenet

import (
	"context"
	"net"
	"testing"
	"time"
)

// TestSpaceNet_ControllerReachesHostService is the reachability spike for the
// Network Controller design: the SAME host address is unreachable from the
// space netns with no controllerIP set, and reachable once it is — proving
// the controller can be dialed from inside a Space the same way allowHost
// already proves a LAN host service can, before any DNS/proxy/registry code
// is built on top of this route.
func TestSpaceNet_ControllerReachesHostService(t *testing.T) {
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
		name         string
		controllerIP string
		want         bool
	}{
		{"denied by the blackhole", "", false},
		{"allowed by the /32 exception", hostIP.String(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cn, cerr := NewSpaceNet("allowcontroller-e2e", "10.88.0.0/24")
			if cerr != nil {
				t.Fatalf("NewSpaceNet: %v", cerr)
			}
			defer cn.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			stop, eerr := cn.EnableEgress(ctx, slirpBin, false, false, nil, tc.controllerIP)
			if eerr != nil {
				t.Skipf("EnableEgress unavailable here: %v", eerr)
			}
			defer stop()

			if got := cn.ProbeTCP(target); got != tc.want {
				t.Fatalf("ProbeTCP(%s) with controllerIP=%q = %v, want %v",
					target, tc.controllerIP, got, tc.want)
			}
		})
	}
}
