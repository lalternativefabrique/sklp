//go:build linux

package spacenet

import (
	"fmt"
	"net"
	"os"
	"slices"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// allowHost punches per-address /32 holes through the RFC1918 blackhole so a
// space service can reach a service listening on the HOST's own LAN address.
// It exists for `sklp dev` only (devstack sets it; deploy and CI never do) —
// the build path's fail-closed createContainer hook has no equivalent and is
// deliberately left without one.
//
// The mechanism is longest-prefix match, the same one that already keeps the
// bridge subnet and the slirp link reachable through the 10.0.0.0/8 blackhole:
// a /32 unicast route is strictly more specific than any of the blackholed
// prefixes, so it wins without removing a single deny route. Nothing else on
// the LAN becomes reachable.
//
// Addresses are resolved at space start, not written in a config: a DHCP lease
// renewal changes the host's address, and a hardcoded one would silently fall
// back into the blackhole.
//
// The default gateway is exempted alongside the interface addresses. On the vz
// backend the Linux host is a VM behind Virtualization.framework's NAT, and the
// developer's machine — the one "host" the flag is meant to reach — is that
// NAT's gateway (192.168.64.1), never an address the VM's own interfaces carry.
// Read from the routing table at the same time as the interfaces, for the same
// reason: it moves with the lease and must not be written down.

// hostAllowAddrs returns the host's own global IPv4 addresses, each as a /32,
// skipping loopback, link-local and every virtual bridge. It runs on the HOST
// stack (before entering the space netns) because that is the only namespace
// where the physical interfaces are visible.
//
// Bridges are excluded by LINK TYPE, not by name: docker0/virbr0/br-<id>/
// podman0 all front OTHER container or VM workloads, so routing to one would
// open a lateral path to those neighbours instead of to the host service the
// user asked for. Matching the type catches the ones we cannot enumerate.
func hostAllowAddrs() ([]string, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("list host links: %w", err)
	}
	var out []string
	for _, l := range links {
		attrs := l.Attrs()
		if attrs.Flags&net.FlagUp == 0 || l.Type() == "bridge" {
			continue
		}
		addrs, aerr := netlink.AddrList(l, unix.AF_INET)
		if aerr != nil {
			continue
		}
		for _, a := range addrs {
			ip := a.IP.To4()
			// Link-local is refused unconditionally: 169.254.169.254 is the
			// cloud metadata endpoint (IAM credential theft), and allowHost
			// must never be a path to it.
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, ip.String()+"/32")
		}
	}
	routes, err := netlink.RouteList(nil, unix.AF_INET)
	if err != nil {
		return nil, fmt.Errorf("list host routes: %w", err)
	}
	if gw := defaultGatewayAllowAddr(routes); gw != "" && !slices.Contains(out, gw) {
		out = append(out, gw)
	}
	return out, nil
}

// defaultGatewayAllowAddr returns the IPv4 default gateway as a /32, or "" when
// the host has none or it is an address allowHost refuses (link-local, loopback).
func defaultGatewayAllowAddr(routes []netlink.Route) string {
	for _, r := range routes {
		if r.Dst != nil {
			if ones, _ := r.Dst.Mask.Size(); ones != 0 {
				continue
			}
		}
		gw := r.Gw.To4()
		if gw == nil || gw.IsLoopback() || gw.IsLinkLocalUnicast() || gw.IsUnspecified() {
			continue
		}
		return gw.String() + "/32"
	}
	return ""
}

// installHostAllowRoutes adds a /32 unicast route for each address via the
// slirp gateway, in the CURRENT netns. It must run AFTER blackholeMeta so the
// more specific route is the one longest-prefix match selects.
//
// Best-effort per address, mirroring blackholeMeta: a failure is logged and
// skipped rather than tearing the space down, because losing one exception
// costs reachability, never isolation.
func installHostAllowRoutes(addrs []string, gw net.IP, linkIndex int) {
	for _, cidr := range addrs {
		_, dst, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		r := &netlink.Route{
			Dst:       dst,
			Gw:        gw,
			LinkIndex: linkIndex,
			Type:      unix.RTN_UNICAST,
			Family:    unix.AF_INET,
		}
		if err := netlink.RouteReplace(r); err != nil {
			fmt.Fprintf(os.Stderr, "[sklp] space egress filter: allowHost %s: %v\n", cidr, err)
		}
	}
}

// hostAllowInstalled reports whether every address in addrs has a non-blackhole
// route in this netns. Used by EgressStatus so a test asserts the exception is
// actually in force instead of merely requested.
func hostAllowInstalled(addrs []string) bool {
	for _, cidr := range addrs {
		_, dst, err := net.ParseCIDR(cidr)
		if err != nil {
			return false
		}
		routes, err := netlink.RouteListFiltered(unix.AF_INET,
			&netlink.Route{Dst: dst}, netlink.RT_FILTER_DST)
		if err != nil || len(routes) == 0 {
			return false
		}
		for _, rt := range routes {
			if rt.Type == unix.RTN_BLACKHOLE {
				return false
			}
		}
	}
	return true
}
