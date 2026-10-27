//go:build linux

package spacenet

import (
	"fmt"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// wgallow_linux.go punches a targeted route through the 10.0.0.0/8
// blackhole to THIS host's own wg0 interface — the Space-side half of the
// cross-host relay (ADR-008). Deliberately narrower than allowHost
// (ADR-005 §5): allowHost exposes every non-bridge host address and is
// gated dev-only; a Space that opts into the mesh relay should only ever
// reach wg0, on any host, dev or deploy alike.

// wgAllowAddr returns the wg0 mesh subnet (e.g. 10.90.0.0/24) THIS host
// belongs to, or "" if wg0 does not exist (mesh not joined) or has no IPv4
// address yet. Resolved on the HOST stack, same as hostAllowAddrs — the
// space netns has no view of the physical/virtual interfaces.
//
// The whole subnet, not this host's own /32: a Space dials a REMOTE peer's
// mesh IP (meshauthority itself, or another node's cross-host relay
// receiver), never its own host's. A /32 of this host's own address routes
// nothing useful — every real destination lives at a different address in
// the same subnet.
func wgAllowAddr() (string, error) {
	link, err := netlink.LinkByName("wg0")
	if err != nil {
		if _, ok := err.(netlink.LinkNotFoundError); ok {
			return "", nil // no mesh joined: not an error, just nothing to allow
		}
		return "", fmt.Errorf("lookup wg0: %w", err)
	}
	addrs, err := netlink.AddrList(link, unix.AF_INET)
	if err != nil {
		return "", fmt.Errorf("list wg0 addresses: %w", err)
	}
	for _, a := range addrs {
		ip := a.IP.To4()
		if ip == nil || ip.IsLoopback() {
			continue
		}
		ones, _ := a.Mask.Size()
		return fmt.Sprintf("%s/%d", a.IPNet.IP.Mask(a.Mask).String(), ones), nil
	}
	return "", nil
}
