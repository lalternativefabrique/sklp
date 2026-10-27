//go:build linux

package spacenet

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/lalternativefabrique/sklp/internal/xdg"
)

// AttachPlan is everything the createRuntime hook needs to attach one container
// onto the space bridge, computed host-side at bundle time — BEFORE the
// container's init pid exists. The host owns IPAM and veth naming; the hook
// fills only the pid (read from the OCI container state) and pushes the plan to
// the worker's attach socket. IP is the bare address for DNS/probes.
type AttachPlan struct {
	Name     string // space DNS service name ("" = no DNS registration)
	IfName   string // in-container interface, e.g. eth0
	CIDR     string // container address with prefix, e.g. 10.88.0.2/24
	GwIP     string // space gateway IP for the default route
	Host     string // host-side veth name (space netns)
	Peer     string // peer veth name before the in-container rename
	IP       string // bare container IP (informational; routing is by name)
	SockPath string // worker attach socket the hook connects to

	// AllowHost is the same /32 exception list EnableEgress already applied to
	// the space's bridge netns (allowHost's LAN addresses, wgAllow's mesh
	// subnet, the Network Controller's address) — mirrored here so the
	// createContainer hook, which configures the peer from INSIDE the
	// container's own netns, punches the identical holes through that
	// netns's own blackholeMeta() instead of leaving it RFC1918-only.
	AllowHost []string
}

// AttachSockReady reports whether the worker's attach socket is wired, i.e.
// whether hook-driven attach (network-before-exec) is available. When false the
// launcher must fall back to the post-start host-side Attach.
func (c *SpaceNet) AttachSockReady() bool { return c.attachSock != "" }

// PlanAttach reserves an IP and veth names for a not-yet-started container and
// returns the AttachPlan the createRuntime hook will execute. It performs the
// host-side bookkeeping of Attach (IPAM + veth naming) WITHOUT touching the
// worker or the container — there is no pid yet. The reservation is recorded on
// BindAttach (once the pid is known) so Detach/Close can undo it; if the
// container never starts, ReleaseAttach returns the IP to the pool.
func (c *SpaceNet) PlanAttach(name, ifName string) (*AttachPlan, error) {
	if ifName == "" {
		ifName = "eth0"
	}
	cidr, ip, err := c.ipam.allocate()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		c.ipam.release(ip)
		return nil, fmt.Errorf("spacenet: %q is closed", c.name)
	}
	c.vethSeq++
	// "ve" + frag(<=5) + seq(<=4) + side(1) <= 12 bytes, under IFNAMSIZ-1.
	frag := nameFrag(c.name, 5)
	host := fmt.Sprintf("ve%s%dh", frag, c.vethSeq)
	peer := fmt.Sprintf("ve%s%dp", frag, c.vethSeq)
	return &AttachPlan{
		Name:      name,
		IfName:    ifName,
		CIDR:      cidr,
		GwIP:      c.ipam.gateway.String(),
		Host:      host,
		Peer:      peer,
		IP:        ip.String(),
		SockPath:  c.attachSock,
		AllowHost: c.allowHostAddrs,
	}, nil
}

// BindAttach records a hook-completed attach against its container pid so
// Detach (and the Close backstop) can later remove the veth and reclaim the IP,
// exactly as the legacy host-side Attach path does. Called by the launcher once
// the container init pid is known (after start) and the hook has succeeded.
func (c *SpaceNet) BindAttach(plan *AttachPlan, containerPID int) {
	if plan == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attached[containerPID] = attachment{host: plan.Host, ip: net.ParseIP(plan.IP), name: plan.Name}
}

// ReleaseAttach returns a planned attach's IP to the pool when the container
// never started (so the hook never ran), avoiding an IPAM leak across a failed
// launch. Idempotent; safe on a closed space.
func (c *SpaceNet) ReleaseAttach(plan *AttachPlan) {
	if plan == nil {
		return
	}
	c.ipam.release(net.ParseIP(plan.IP))
}

// CancelAttach unwinds a plan whose hook may already have completed but whose
// runc launch still failed afterwards. It removes the DNS entry and veth
// best-effort before reclaiming the IP, so a failed create cannot leave a
// reachable stale peer behind.
func (c *SpaceNet) CancelAttach(plan *AttachPlan) {
	if plan == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		ip := net.ParseIP(plan.IP)
		if plan.Name != "" {
			_, _ = c.send(command{Op: "unregister", Name: plan.Name, IP: ip.String()})
		}
		_, _ = c.send(command{Op: "detach", Host: plan.Host})
	}
	c.ipam.release(net.ParseIP(plan.IP))
}

// sockDir returns the 0700 per-user directory holding the space control
// sockets, creating it if needed. These sockets are UNAUTHENTICATED command
// channels: attach mutates the space's netns, and ingress splices a caller-named
// service:port from INSIDE the netns — so anyone who can connect reaches every
// service in the space, including ones no route exposes. Their names are
// sha256(space)[:12], derivable by anyone who knows the space name, so the
// directory mode is the whole access control. A world-writable base (os.TempDir)
// left them connectable by any local user and squattable ahead of bind; the
// per-user runtime dir is 0700 and owned by us. Returns "" when the dir cannot
// be secured, which disables the FS sockets rather than exposing them.
func sockDir() string {
	dir := xdg.RuntimeRunRoot()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	// MkdirAll leaves a pre-existing dir's mode untouched; force it back to 0700
	// so an earlier looser creation cannot keep the sockets exposed.
	if err := os.Chmod(dir, 0o700); err != nil {
		return ""
	}
	fi, err := os.Stat(dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		return ""
	}
	return dir
}

// attachSockPath is the space-private path of the worker's attach control
// socket, under the 0700 runtime dir (see sockDir). Short (a unix socket path is
// capped near 108 bytes) and stable per space. Returns "" if the dir is
// unusable, which disables hook-driven attach (the launcher then falls back to
// Attach).
func attachSockPath(name string) string {
	dir := sockDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "space-attach-"+nameFrag(name, 12)+".sock")
}

// ingressSockPath is the space-private path of the worker's ingress control
// socket (the gateway mux connects here to fd-pass client connections). Same
// 0700 runtime-dir layout as attachSockPath, distinct prefix. Returns "" if the
// dir is unusable. Deterministic per space name so a separate mux process can
// derive it.
func ingressSockPath(name string) string {
	dir := sockDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "space-ingress-"+nameFrag(name, 12)+".sock")
}

// IngressSockPath returns the deterministic ingress control socket path for a
// space, derived from its name alone — so the gateway mux (a separate process)
// can reach a running space's worker without sharing any fd. Exported for the
// mux; the value matches what the worker listens on.
func IngressSockPath(name string) string { return ingressSockPath(name) }
