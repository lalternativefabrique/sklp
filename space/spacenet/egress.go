//go:build linux

package spacenet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lalternativefabrique/sklp/space/ociproc"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// slirpTap is the interface slirp4netns creates in the space netns; it is the
// egress device every bridge-subnet packet is NAT'd out of. slirp gives it
// 10.0.2.100/24 with a default route via 10.0.2.2 (the slirp gateway).
const slirpTap = "tap0"

// EnableEgress gives the whole space bridge subnet outbound Internet by
// attaching slirp4netns to the worker's netns (creating tap0 + a default route
// via the slirp gateway) and then NAT'ing the bridge subnet out of tap0 inside
// the space netns. Once it succeeds the in-space resolver can forward
// external names upstream (they previously SERVFAIL'd for lack of egress).
//
// It is best-effort, mirroring the CI slirp path: a missing/failing slirp
// binary, a kernel without /dev/net/tun, or absent nft all return an error the
// caller logs as a warning. Egress is then simply unavailable — the internal
// bridge (sibling reachability + local DNS) keeps working untouched. It NEVER
// tears the space down. The returned stop func kills slirp on teardown; it is
// nil when egress could not be brought up.
func (c *SpaceNet) EnableEgress(ctx context.Context, slirpBin string, allowHost, allowMesh bool, allowEgress []string, controllerIP string) (stop func(), err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, fmt.Errorf("spacenet: %q is closed", c.name)
	}
	if c.worker == nil || c.worker.Process == nil {
		return nil, fmt.Errorf("spacenet: %q has no worker", c.name)
	}

	// Attach slirp to the worker's netns (the space netns). slirp re-originates
	// egress from the HOST stack; the bridge subnet is private, so the only
	// addresses leaving the host are the host's own — no extra exposure.
	stop, err = attachSlirp(ctx, slirpBin, c.worker.Process.Pid)
	if err != nil {
		return nil, fmt.Errorf("spacenet: attach slirp: %w", err)
	}

	// Inside the space netns, route the bridge subnet to tap0: enable IPv4
	// forwarding and MASQUERADE the subnet out of tap0. Done by the worker, which
	// is root in the space userns and so may set the net-namespaced sysctl and
	// program nft. If this fails, drop slirp too — a tap0 with no NAT is dead
	// weight, and the internal network is unaffected either way.
	// Resolve the host's own addresses HERE, on the host stack: the worker netns
	// has no view of the physical interfaces. Resolved per space start, so a DHCP
	// lease renewal is picked up instead of silently falling back into the
	// blackhole. A resolution failure degrades to no exception, never to a
	// weaker filter.
	var allowAddrs []string
	if allowHost {
		var aerr error
		if allowAddrs, aerr = hostAllowAddrs(); aerr != nil {
			fmt.Fprintf(os.Stderr, "[sklp] space egress filter: allowHost: %v\n", aerr)
			allowAddrs = nil
		}
	}
	// A Space that declared a cross-host relay dependency (ADR-008) also
	// gets a route to THIS host's own wg0 — never to the rest of the mesh,
	// which only the worker's cross-host relay receiver (bound to wg0) is
	// meant to reach on its behalf.
	if allowMesh {
		if wgAddr, werr := wgAllowAddr(); werr != nil {
			fmt.Fprintf(os.Stderr, "[sklp] space egress filter: allowMesh: %v\n", werr)
		} else if wgAddr != "" {
			allowAddrs = append(allowAddrs, wgAddr)
		}
	}
	// Every Space with egress reaches the Network Controller unconditionally:
	// a service's own port: declaration is what gates registration/reachability,
	// not a per-Space opt-in (see allowcontroller_linux.go).
	if ctrlAddr := controllerAllowAddr(controllerIP); ctrlAddr != "" {
		allowAddrs = append(allowAddrs, ctrlAddr)
	}
	c.allowHostAddrs = allowAddrs

	rep, serr := c.send(command{Op: "egress", CIDR: c.ipam.network.String(), IF: slirpTap, AllowHost: allowAddrs, AllowEgress: allowEgress})
	if serr != nil || !rep.OK {
		stop()
		if serr != nil {
			return nil, serr
		}
		return nil, fmt.Errorf("spacenet: program egress NAT: %s", rep.Error)
	}

	// `sklp dev` asks for the host on purpose (allowHost); everywhere else a
	// container is kept off its own node except for the controller's ports.
	// Fail closed: a space whose guard cannot be installed does not get
	// egress at all.
	if !allowHost {
		hostAddrs, herr := hostOwnAddrs()
		if herr != nil {
			stop()
			return nil, fmt.Errorf("spacenet: host guard: %w", herr)
		}
		var ctrlAddrs []string
		if controllerIP != "" {
			ctrlAddrs = append(ctrlAddrs, controllerIP)
		}
		if m := ownMeshAddr(); m != "" {
			ctrlAddrs = append(ctrlAddrs, m)
		}
		c.hostGuard = true
		c.guardHostAddrs = hostAddrs
		c.guardControllerAddrs = ctrlAddrs
		c.guardPublicAddrs = nodePublicAddrs()
		grep, gerr := c.send(command{Op: "hostguard", IF: slirpTap, HostAddrs: hostAddrs, ControllerAddrs: ctrlAddrs, PublicAddrs: c.guardPublicAddrs, Ports: currentControllerPorts()})
		if gerr != nil || !grep.OK {
			stop()
			if gerr != nil {
				return nil, fmt.Errorf("spacenet: host guard: %w", gerr)
			}
			return nil, fmt.Errorf("spacenet: host guard: %s", grep.Error)
		}
		trackHostGuard(c, true)
	}

	// Own the teardown here so Close reaps slirp even if the caller drops the
	// returned func. The wrapper invokes the underlying kill exactly once via a
	// sync.Once, so a double stop (caller + Close) is harmless. NB: capture the
	// slirp killer in its own variable, NOT the named return `stop` (which we
	// overwrite with `guarded` below — closing over it would self-recurse).
	killSlirp := stop
	var once sync.Once
	guarded := func() { once.Do(killSlirp) }
	c.stopEgress = guarded
	return guarded, nil
}

// attachSlirp starts slirp4netns against pid's netns (tap0 + 10.0.2.100/24 +
// default route via 10.0.2.2). It mirrors runtime.startSlirp but lives here so
// spacenet stays free of an import cycle on its parent package. Best-effort: an
// empty path (extraction failed) or a start error returns an error the caller
// turns into a warning.
func attachSlirp(ctx context.Context, slirpBin string, pid int) (stop func(), err error) {
	if slirpBin == "" {
		return nil, fmt.Errorf("slirp4netns binary unavailable")
	}
	// --configure: tap0 + IP + default route inside the target netns.
	// --mtu=1500: standard Ethernet MTU. A jumbo MTU (65520) maximises
	// loopback throughput but breaks real egress: large TCP transfers
	// (apt/skt downloads) leave the netns in 64K frames the physical link
	// (MTU 1500) cannot carry, and with no working path-MTU discovery across
	// slirp they are dropped — the download hangs forever. Egress here is
	// Internet downloads, not loopback, so 1500 is the correct trade.
	// --disable-host-loopback: the space cannot reach the host's loopback.
	cmd := exec.CommandContext(ctx, slirpBin,
		"--configure", "--mtu=1500", "--disable-host-loopback",
		strconv.Itoa(pid), slirpTap)
	// slirp4netns prints startup chatter ("sent/received tapfd=N for tap0") on
	// both streams. Inheriting either one writes to the terminal a live progress
	// block owns, desyncing its cursor rewind into an endlessly restacking
	// spinner, so buffer it and surface it only when slirp fails to start.
	var slirpLog ociproc.SlirpLog
	cmd.Stdout = &slirpLog
	cmd.Stderr = &slirpLog
	// slirp runs on the HOST stack, not the worker netns, so neither the worker
	// dying nor ctx cancellation reaps it on a SIGKILL of the supervisor (a
	// SIGKILL'd process runs no ctx cancel). Pdeathsig has the kernel kill slirp
	// when the supervisor (its parent) dies, however it dies. reapOrphanSlirp
	// stays the backstop for the residual fork-window gap.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if err := startPinned(cmd); err != nil {
		return nil, err
	}
	if err := slirpSurvivesStartup(cmd, &slirpLog); err != nil {
		return nil, err
	}
	// Give slirp a moment to wire tap0 + the default route before we program NAT
	// against it. Mirrors the CI path; a --ready-fd handshake is the proper fix
	// once egress timing matters.
	time.Sleep(300 * time.Millisecond)

	return func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}, nil
}

// egressTable is the nft table the worker owns for space egress NAT. A named,
// space-private table (the worker netns is destroyed on Close) keeps the rule
// self-describing and trivially torn down with the namespace.
const egressTable = "sklp_spacenet_egress"

// programEgress runs INSIDE the worker (space netns, root in the space
// userns). It enables IPv4 forwarding and MASQUERADEs the bridge subnet out of
// the slirp tap, so a container's default route (-> bridge gw -> forwarded ->
// tap0) reaches the Internet and replies are NAT'd back. subnet is the bridge
// CIDR (e.g. 10.88.0.0/24); tap is the slirp egress interface (tap0).
//
// The net-namespaced ip_forward sysctl and nft are both writable here because
// the worker is uid 0 in its own user namespace with CAP_NET_ADMIN over the
// space netns — no real root, no host change. nft programming is best-effort
// from the host's view: an error is returned so EnableEgress can drop slirp and
// warn, leaving the internal network intact.
func programEgress(subnet, tap string, allowHostAddrs, allowEgress []string) error {
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("enable ip_forward: %w", err)
	}
	// One atomic ruleset: flush any prior table (idempotent re-enable) then add
	// the postrouting masquerade for the bridge subnet leaving via tap.
	script := fmt.Sprintf(
		"add table ip %[1]s\n"+
			"flush table ip %[1]s\n"+
			"add chain ip %[1]s postrouting { type nat hook postrouting priority 100 ; }\n"+
			"add rule ip %[1]s postrouting ip saddr %[2]s oifname \"%[3]s\" masquerade\n",
		egressTable, subnet, tap)
	// A declared allowlist turns forwarding into default-deny: only the named
	// destinations leave the space. Appended to the SAME atomic ruleset as the
	// masquerade so a container is never briefly forwarded unfiltered.
	script += allowEgressRules(subnet, tap, allowEgress)
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nft masquerade %s -> %s: %w (%s)", subnet, tap, err, strings.TrimSpace(string(out)))
	}
	// Blackhole the IMDS link-local + RFC1918 ranges in this space netns. Done
	// AFTER the egress default route exists so longest-prefix match keeps the
	// specific routes alive while the broad ranges are denied. See blackholeMeta.
	blackholeMeta()
	// `sklp dev` only: re-open the host's own /32s, which the RFC1918 blackhole
	// just denied. Strictly after blackholeMeta so the more specific route wins
	// longest-prefix match. Empty on every other path — deploy and CI never set
	// it, so the filter there is unchanged.
	if len(allowHostAddrs) > 0 {
		if gw, idx, err := slirpGateway(tap); err == nil {
			installHostAllowRoutes(allowHostAddrs, gw, idx)
		} else {
			fmt.Fprintf(os.Stderr, "[sklp] space egress filter: allowHost gateway: %v\n", err)
		}
	}
	return nil
}

// isDefaultRoute reports whether dst denotes the default route. netlink renders
// it either as a nil Dst or as an explicit 0.0.0.0/0, depending on how the route
// was installed, so both spellings must be accepted.
func isDefaultRoute(dst *net.IPNet) bool {
	if dst == nil {
		return true
	}
	ones, _ := dst.Mask.Size()
	return ones == 0
}

// slirpGateway returns the gateway IP and link index of the default route slirp
// installed on tap, so the allowHost /32s can be pointed at the same next hop.
// Read from the routing table rather than hardcoding slirp's 10.0.2.2, which is
// a slirp implementation detail we do not otherwise depend on.
func slirpGateway(tap string) (net.IP, int, error) {
	link, err := netlink.LinkByName(tap)
	if err != nil {
		return nil, 0, fmt.Errorf("tap %s: %w", tap, err)
	}
	idx := link.Attrs().Index
	// Scan the whole table, not RouteList(link, …): that filters on oif, and the
	// default route slirp --configure installs is not always attributed to the
	// tap that way. Match on "no Dst" (the default) instead, preferring one that
	// does leave via the tap.
	routes, err := netlink.RouteList(nil, unix.AF_INET)
	if err != nil {
		return nil, 0, fmt.Errorf("route table: %w", err)
	}
	var fallback net.IP
	for _, r := range routes {
		if r.Gw == nil || !isDefaultRoute(r.Dst) {
			continue
		}
		if r.LinkIndex == idx {
			return r.Gw, idx, nil
		}
		if fallback == nil {
			fallback = r.Gw
		}
	}
	if fallback != nil {
		return fallback, idx, nil
	}
	// No gateway anywhere: a /32 with only an output interface still reaches the
	// host stack across slirp's tap, so hand back the index with a nil gateway.
	return nil, idx, nil
}

// blackholeMetadataCIDRs is the single source of truth shared with the build
// path (ociproc.BlackholeMetadataCIDRs): the cloud metadata link-local (IMDS,
// 169.254.0.0/16) plus the three RFC1918 host-LAN ranges. Aliased here so the
// space netns and the build netns can never drift to different filter sets.
//
// The space bridge subnet (e.g. 10.88.0.0/24) and the slirp tap (10.0.2.0/24)
// are CONNECTED /24 routes, strictly more specific than the 10.0.0.0/8
// blackhole, so longest-prefix match keeps east-west traffic and the DNS
// gateway reachable; only addresses with no more-specific route (IMDS, the
// host LAN) hit the blackhole.
var blackholeMetadataCIDRs = ociproc.BlackholeMetadataCIDRs

// blackholeMeta installs the deny routes inside the current (space) netns. It
// runs in the worker, which holds CAP_NET_ADMIN over this netns. Best-effort and
// idempotent: RouteReplace overwrites an identical prior entry on re-deploy, and
// a per-route failure is logged and skipped — egress filtering is
// defence-in-depth, not a hard gate (mirrors slirp's best-effort blackhole).
func blackholeMeta() {
	for _, cidr := range blackholeMetadataCIDRs {
		_, dst, err := net.ParseCIDR(cidr)
		if err != nil {
			continue // compile-time constant CIDRs; never hit
		}
		r := &netlink.Route{Dst: dst, Type: unix.RTN_BLACKHOLE, Family: unix.AF_INET}
		if err := netlink.RouteReplace(r); err != nil {
			fmt.Fprintf(os.Stderr, "[sklp] space egress filter: blackhole %s: %v\n", cidr, err)
		}
	}
}

// EgressStatus reports the space egress wiring as observed inside the space
// netns: whether the slirp tap is up, IPv4 forwarding is on, the masquerade rule
// is installed, and the IMDS/RFC1918 blackhole deny routes are present. It exists
// so a test can assert egress is configured without entering the worker's
// namespaces. All are false when egress was never enabled.
func (c *SpaceNet) EgressStatus() (tapUp, forwarding, masquerade, blackhole bool, err error) {
	st, err := c.EgressState()
	if err != nil {
		return false, false, false, false, err
	}
	return st.TapUp, st.Forwarding, st.Masquerade, st.Blackhole, nil
}

// EgressState is the full egress wiring observed inside the space netns.
// AllowHost reports whether every host /32 requested via `sklp dev` allowHost
// is routed around the blackhole; it is true when none was requested.
type EgressState struct {
	TapUp      bool
	Forwarding bool
	Masquerade bool
	Blackhole  bool
	AllowHost  bool
}

// EgressState reports the space egress wiring, including the allowHost
// exceptions EgressStatus predates and cannot express.
func (c *SpaceNet) EgressState() (EgressState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return EgressState{}, fmt.Errorf("spacenet: %q is closed", c.name)
	}
	rep, err := c.send(command{Op: "egressStatus", IF: slirpTap, AllowHost: c.allowHostAddrs})
	if err != nil {
		return EgressState{}, err
	}
	if !rep.OK {
		return EgressState{}, fmt.Errorf("spacenet: egress status: %s", rep.Error)
	}
	return EgressState{
		TapUp:      rep.TapUp,
		Forwarding: rep.Forwarding,
		Masquerade: rep.Masquerade,
		Blackhole:  rep.Blackhole,
		AllowHost:  rep.AllowHost,
	}, nil
}

// egressStatus runs inside the worker and inspects the space netns: the tap's
// presence+up flag (netlink), the net-namespaced ip_forward sysctl, and whether
// the egress nft table carries a masquerade rule.
func egressStatus(tap string, allowHostAddrs []string) reply {
	r := reply{OK: true}
	if l, err := netlink.LinkByName(tap); err == nil {
		r.TapUp = l.Attrs().Flags&net.FlagUp != 0
	}
	if b, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward"); err == nil {
		r.Forwarding = strings.TrimSpace(string(b)) == "1"
	}
	out, err := exec.Command("nft", "list", "table", "ip", egressTable).CombinedOutput()
	if err == nil && strings.Contains(string(out), "masquerade") {
		r.Masquerade = true
	}
	r.Blackhole = blackholeInstalled()
	r.AllowHost = hostAllowInstalled(allowHostAddrs)
	return r
}

// peerBlackhole runs inside the worker: it hops into the peer's netns (the
// container side), checks the IMDS/RFC1918 deny routes are present there, then
// returns to the space netns. Unlike egressStatus (which inspects the worker
// netns), this asserts the SOURCE-side reject configurePeer installs on attach.
func peerBlackhole(capNS netns.NsHandle, srcPID int) (ok bool, err error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	srcNS, err := netns.GetFromPid(srcPID)
	if err != nil {
		return false, fmt.Errorf("open peer netns (pid %d): %w", srcPID, err)
	}
	defer srcNS.Close()
	if err := netns.Set(srcNS); err != nil {
		return false, fmt.Errorf("enter peer netns: %w", err)
	}
	defer func() {
		if setErr := netns.Set(capNS); setErr != nil && err == nil {
			err = fmt.Errorf("return to space netns: %w", setErr)
		}
	}()
	return blackholeInstalled(), nil
}

// blackholeInstalled reports whether every IMDS/RFC1918 deny route is present in
// this netns as a blackhole. Used by EgressStatus so a test asserts the SSRF
// guard is wired without entering the space namespaces.
func blackholeInstalled() bool {
	for _, cidr := range blackholeMetadataCIDRs {
		_, dst, err := net.ParseCIDR(cidr)
		if err != nil {
			return false
		}
		routes, err := netlink.RouteListFiltered(unix.AF_INET,
			&netlink.Route{Dst: dst}, netlink.RT_FILTER_DST)
		if err != nil {
			return false
		}
		found := false
		for _, rt := range routes {
			if rt.Type == unix.RTN_BLACKHOLE {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// slirpSurvivesStartup gives slirp4netns its first 300 ms, then reports an
// exit in that window as the error it is: a dead slirp is a space with no
// Internet, and the first sign of that used to be a DNS failure minutes later.
//
// An ECHILD here is NOT that: it means the supervisor's SIGCHLD subreaper (see
// ociproc.runTolerateECHILD) reaped slirp4netns before this Wait could, which
// says nothing about whether slirp itself is still alive. Treating it as a
// startup crash would kill a space whose network is fine.
func slirpSurvivesStartup(cmd *exec.Cmd, log *ociproc.SlirpLog) error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if errors.Is(err, unix.ECHILD) {
			return nil
		}
		return fmt.Errorf("slirp4netns exited during startup (%v): %s", err, log.String())
	case <-time.After(300 * time.Millisecond):
		return nil
	}
}
