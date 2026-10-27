//go:build linux

package ociproc

import (
	"fmt"
	"net"
	"os"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// The egress blackhole is installed by an OCI **createContainer** hook, NOT by
// the host (the previous nsenter-from-host approach silently failed with EPERM
// on a rootless netns owned by a child user namespace — issue #610).
//
// Why createContainer specifically (proven empirically, runc 1.4.2 rootless):
//   - prestart / createRuntime run in the RUNTIME namespace on the host side,
//     outside the container's child userns → no CAP_NET_ADMIN over its netns →
//     the route add fails (silently, when best-effort).
//   - createContainer is exec'd by runc AFTER it has placed the hook process
//     INSIDE the container's namespaces (user + net). So the hook already holds
//     CAP_NET_ADMIN over that netns and can add routes with plain netlink — no
//     setns, no cgo, no LockOSThread, no userns-before-threading constraint.
//
// The hook is a re-exec of the sklp binary itself: runc runs `<self> ` with the
// marker env set; MaybeRunBlackholeHook takes over and exits. A non-zero exit
// FAILS `runc create`, so a workload whose egress filter could not be installed
// never runs — fail-closed, the structural fix for the silent best-effort hole.

const blackholeHookEnv = "SKLP_BLACKHOLE_HOOK"

// BlackholeMetadataCIDRs are the egress-deny destinations: the IMDS link-local
// range (includes 169.254.169.254, the cloud metadata endpoint — IAM cred theft
// vector) plus the three RFC1918 host-LAN ranges. slirp re-originates egress
// from the host stack and has no CIDR filter of its own, so these MUST be
// blackholed inside the netns. The slirp link subnet (10.0.2.0/24) is more
// specific and is installed by slirp's --configure, so longest-prefix match
// keeps the link + its DNS (10.0.2.3) reachable while 10.0.0.0/8 is denied.
var BlackholeMetadataCIDRs = []string{
	"169.254.0.0/16",
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
}

// BlackholeHookSpec returns the OCI `hooks` block that wires the createContainer
// blackhole hook: a re-exec of selfExe with the marker env set. Merge it into a
// spec's "hooks" key (or pass to addHooks). selfExe is the absolute path to the
// running sklp binary (os.Executable()).
func BlackholeHookSpec(selfExe string) map[string]any {
	return map[string]any{
		"createContainer": []map[string]any{
			{
				"path": selfExe,
				"args": []string{selfExe, "blackhole-hook"},
				"env":  []string{blackholeHookEnv + "=1"},
			},
		},
	}
}

// MaybeRunBlackholeHook turns the current process into the createContainer
// blackhole hook when the marker env is set, then exits — it never returns in
// that case. On a normal invocation it is a no-op. Hosts MUST call it from
// main() before any other work, alongside the other hook re-execs.
//
// It runs inside the container's netns (runc placed it there), so it just adds
// the blackhole routes with netlink. Fail-closed: any route error exits non-zero
// → runc create fails → the workload never runs without the filter.
func MaybeRunBlackholeHook() {
	if os.Getenv(blackholeHookEnv) == "" {
		return
	}
	if err := installBlackholeRoutes(); err != nil {
		fmt.Fprintf(os.Stderr, "sklp blackhole hook: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// installBlackholeRoutes adds an RTN_BLACKHOLE route for every CIDR in the
// CURRENT network namespace (the container's, when run as the createContainer
// hook). RouteReplace is idempotent. A failure on any CIDR is returned so the
// caller can fail the run.
func installBlackholeRoutes() error {
	for _, cidr := range BlackholeMetadataCIDRs {
		_, dst, err := net.ParseCIDR(cidr)
		if err != nil {
			return fmt.Errorf("parse %s: %w", cidr, err) // compile-time constants; never hit
		}
		r := &netlink.Route{Dst: dst, Type: unix.RTN_BLACKHOLE, Family: unix.AF_INET}
		if err := netlink.RouteReplace(r); err != nil {
			return fmt.Errorf("blackhole %s: %w", cidr, err)
		}
	}
	return nil
}
