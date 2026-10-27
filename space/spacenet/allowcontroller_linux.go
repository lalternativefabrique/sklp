//go:build linux

package spacenet

import (
	"fmt"
	"strings"
)

// allowcontroller_linux.go punches a targeted /32 route through the
// 10.0.0.0/8 blackhole to the host's Network Controller — the one process,
// external to every Space, that owns all cross-Space DNS/HTTP/TCP traffic.
// Reuses the exact mechanism allowHost/wgAllow already prove in production
// (longest-prefix-match beats the blackhole); this is a third caller of that
// same installHostAllowRoutes, not a new mechanism.
//
// Unlike allowHost (sklp dev only, opt-in) this is meant to be unconditional:
// every Space with egress enabled gets this route, since DNS resolvability
// and controller reachability are the same trust boundary once a service's
// own port: is what gates whether it is registered at all.
//
// A loopback alias (e.g. 127.0.0.2) was spiked and rejected: slirp4netns
// blackholes the entire loopback range regardless of exception routes (its
// own SSRF-toward-the-host guard), so the controller MUST bind a real host
// interface address, exactly like allowHost.

// EnvControllerIP is the env var the host process sets (sklet.RunAgent) and
// the re-exec'd worker reads (hostUpstreams, dns.go) so a Space's DNS
// forwards .internal queries to the controller. Exported: apps/sklp/space's
// deploynet_linux.go (a sibling package) reads the same variable when
// building the /32 egress exception, so both call sites share one name.
const EnvControllerIP = "SKLP_CONTROLLER_IP"

const envControllerIP = EnvControllerIP

// controllerAllowAddr returns the controller's fixed IP as a /32, or "" if
// no controller address is configured. Resolved on the HOST stack (same
// namespace-visibility constraint as hostAllowAddrs/wgAllowAddr) even though
// the address itself is fixed, so the call site stays uniform with its two
// siblings and the door is open for the controller's address to become
// dynamic later (e.g. resolved from a running Registry instance) without
// changing this function's signature.
func controllerAllowAddr(controllerIP string) string {
	if controllerIP == "" {
		return ""
	}
	return controllerIP + "/32"
}

// ControllerHostIP picks the address the Network Controller should bind to:
// the first non-bridge, non-loopback, non-link-local global IPv4 address on
// the host, mirroring hostAllowAddrs' own filter exactly — a controller
// address that hostAllowAddrs itself would refuse to exempt could never be
// reachable through the same blackhole either.
func ControllerHostIP() (string, error) {
	addrs, err := hostAllowAddrs()
	if err != nil {
		return "", fmt.Errorf("spacenet: pick controller host IP: %w", err)
	}
	if len(addrs) == 0 {
		return "", fmt.Errorf("spacenet: no host IPv4 address available for the Network Controller")
	}
	// hostAllowAddrs returns "<ip>/32" entries; strip the mask.
	ip, _, _ := strings.Cut(addrs[0], "/")
	return ip, nil
}
