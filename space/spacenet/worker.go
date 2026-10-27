//go:build linux

package spacenet

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// workerEnv is the marker NewSpaceNet sets on the re-exec'd child. The host
// binary calls MaybeRunWorker early in main() (or TestMain); when the marker
// is present the process becomes the space worker and never returns.
const workerEnv = "SKLP_SPACE_WORKER"

// envBridge / envGatewayCIDR pass the bridge name and gateway address to the
// worker so the host side owns naming and IPAM.
const (
	envBridge      = "SKLP_SPACE_BRIDGE"
	envGatewayCIDR = "SKLP_SPACE_GW_CIDR"
)

// envSyncFD carries the fd number of the subuid sync pipe's read end (an
// ExtraFile). It is set only on the range-mapping path: the worker is cloned
// into an EMPTY userns and must block reading this fd until the parent has
// written its uid_map/gid_map via newuidmap/newgidmap. The parent then closes
// its end; the read returns EOF and the worker proceeds with a fully mapped
// userns. Absent => single-id mapping was applied at clone time, no barrier.
const envSyncFD = "SKLP_SPACE_SYNC_FD"

// envSupervisorPID carries the supervisor's pid to the worker, so the orphan
// check compares against it rather than against 1: in a container the
// supervisor may itself be pid 1, and a worker that took "parent is pid 1"
// for "parent is dead" exited before ever signalling ready.
const envSupervisorPID = "SKLP_SPACE_SUPERVISOR_PID"

// command is one line-delimited JSON request the host sends to the worker.
type command struct {
	Op   string `json:"op"`             // attach|detach|register|unregister|ping|resolve|spawnsim|egress|egressStatus|acl|peerBlackhole|dial|probeTCP|probeHTTP|close
	PID  int    `json:"pid,omitempty"`  // container init pid (attach/detach)
	IF   string `json:"if,omitempty"`   // in-container interface name (attach); egress tap name (egress); bridge name (acl)
	CIDR string `json:"cidr,omitempty"` // container address, e.g. 10.88.0.2/24 (attach); bridge subnet (egress)
	Host string `json:"host,omitempty"` // host-side veth name (space netns)
	Peer string `json:"peer,omitempty"` // peer veth name before rename
	GwIP string `json:"gw,omitempty"`   // gateway IP for the default route
	Dst  string `json:"dst,omitempty"`  // ping target IP (ping op)
	Name string `json:"name,omitempty"` // service name for the DNS table (register/unregister)
	IP   string `json:"ip,omitempty"`   // service IP for the DNS table (register/unregister)

	// spawnoverlay op only: the per-service overlayfs dirs the holder mounts.
	Lower  string `json:"lower,omitempty"`  // lowerdir: shared digest rootfs (RO)
	Upper  string `json:"upper,omitempty"`  // upperdir: per-service scratch (RW)
	Work   string `json:"work,omitempty"`   // overlayfs workdir (sibling of upper)
	Merged string `json:"merged,omitempty"` // mountpoint = runc root.path
	// ChildRoot, when set, puts the holder in a child user namespace whose
	// root is this worker id and which maps ids 1..ChildCount (see childUserns).
	ChildRoot  int `json:"childRoot,omitempty"`
	ChildCount int `json:"childCount,omitempty"`
	// LockSockDir asks the worker to keep the attach socket's directory to the
	// operator and the child root: group of the child root, mode 0710.
	LockSockDir bool `json:"lockSockDir,omitempty"`

	// l7routes op only: the native L7 ingress routing table (JSON-encoded
	// []ingress.Route), applied to the in-worker reverse proxy at runtime.
	Routes string `json:"routes,omitempty"`

	// egress / egressStatus ops only: host /32s to route around the RFC1918
	// blackhole (`sklp dev` allowHost). Resolved host-side and passed in
	// because the worker netns cannot see the host's physical interfaces.
	AllowHost []string `json:"allowHost,omitempty"`

	// egress op only: the CIDRs a container may reach on the Internet. Empty
	// (the default) keeps egress open, so an existing space is unchanged; a
	// non-empty list turns the forward chain into a default-deny allowlist.
	AllowEgress []string `json:"allowEgress,omitempty"`

	// hostguard op only: every IPv4 address the node owns, the addresses the
	// network controller listens on, and the TCP ports it serves there.
	HostAddrs       []string `json:"hostAddrs,omitempty"`
	ControllerAddrs []string `json:"controllerAddrs,omitempty"`
	Ports           []int    `json:"ports,omitempty"`
	PublicAddrs     []string `json:"publicAddrs,omitempty"`

	// acl op only: the declared depends_on edges (JSON-encoded []aclEdge) this
	// Space's east-west forward chain should allow. Empty (the default) keeps
	// east-west open, so an existing Space is unchanged; a non-empty list
	// turns the bridge's forward chain into a default-deny allowlist.
	Edges string `json:"edges,omitempty"`
}

// reply is the worker's line-delimited JSON response. PID is set only by the
// spawnsim op (the host-visible pid of a netns holder in the space userns).
type reply struct {
	OK    bool     `json:"ok"`
	Error string   `json:"error,omitempty"`
	PID   int      `json:"pid,omitempty"`
	IPs   []string `json:"ips,omitempty"` // A-record answers (resolve op)

	// egressStatus op only: the space egress configuration as observed inside
	// the space netns, so a test asserts the wiring without entering it.
	TapUp      bool `json:"tap_up,omitempty"`     // slirp tap exists and is up
	Forwarding bool `json:"forwarding,omitempty"` // net.ipv4.ip_forward == 1
	Masquerade bool `json:"masquerade,omitempty"` // nft masquerade rule present
	Blackhole  bool `json:"blackhole,omitempty"`  // all IMDS/RFC1918 deny routes present
	AllowHost  bool `json:"allow_host,omitempty"` // every requested host /32 routed around the blackhole
}

// MaybeRunWorker turns the current process into the space network worker
// when the re-exec marker is set, then exits — it never returns in that case.
// In the normal (non-worker) process it is a no-op and returns immediately.
// Hosts that create SpaceNets must call it before any other work in main().
func MaybeRunWorker() {
	if os.Getenv(holderEnv) == "1" {
		park() // never returns
	}
	if os.Getenv(overlayEnv) == "1" {
		if os.Getenv(overlayChildEnv) == "1" {
			runOverlayChildStage1() // never returns: execve's the mapped stage
		}
		runOverlayHolder() // mounts the service overlay, then parks; never returns
	}
	switch os.Getenv(workerEnv) {
	case "1":
		// Stage 1 (range path only): the userns was cloned EMPTY. Wait for the
		// parent to write our uid/gid maps, then re-exec into stage 2 so the new
		// program picks up the userns capabilities (caps are granted at the
		// execve boundary; the unmapped stage-1 image has CapEff=0). re-execs.
		runStage1() // never returns: execve's stage 2 or exits on failure
	case "2":
		// Stage 2: fully mapped userns, full caps — run the worker logic.
		runWorker()
		os.Exit(0)
	}
}

// workerReq is one unit of work for the worker's single netns-pinned thread.
// Exactly one of stdin/attach is set: a stdin JSON command (host pipe) or a
// hook-driven attach (the createRuntime hook's attach socket). Both mutate the
// space netns, so they are funnelled onto one thread to serialise the
// netns/netlink ops without locking — the reply is returned on resp.
type workerReq struct {
	stdin  *command           // a host pipe command (nil for an attach req)
	attach *hookAttachRequest // a hook attach (nil for a stdin command)
	resp   chan<- reply       // where the executed reply is sent
	stop   bool               // stdin closed: parent gone, tear the worker down
}

// runWorker builds the bridge in the netns it was cloned into, then serves
// commands until stdin closes (parent gone => teardown is implicit: exiting
// destroys the netns and every veth/bridge in it). Stdin commands and
// hook-driven attaches (from the attach socket) are funnelled onto this single
// pinned thread so every netns/netlink mutation is serialised.
func runWorker() {
	runtime.LockOSThread() // pin to the space netns for every netlink op

	// Re-arm parent-death-signal. The parent set Pdeathsig at clone, but the
	// kernel CLEARS it across the stage1->stage2 execve (the userns cap pickup
	// changes our dumpability), so without this the worker survives a SIGKILL'd
	// supervisor — the observed kill -9 leak. Then re-check the parent is still
	// alive: if it died during the stage1/execve window the signal was already
	// lost (Pdeathsig does not fire retroactively), so a getppid()==1 means we
	// are already orphaned and must exit ourselves.
	if err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(unix.SIGKILL), 0, 0, 0); err == nil {
		if supervisorGone() {
			os.Exit(0) // supervisor died before we re-armed; do not leak
		}
	}

	out := bufio.NewWriter(os.Stdout)

	capNS, err := netns.Get()
	if err != nil {
		writeReply(out, reply{Error: fmt.Sprintf("get space netns: %v", err)})
		os.Exit(1)
	}

	// Build the bridge, then emit a single readiness reply so the host knows
	// the netns is live (or why it failed) before it sends any command.
	gwCIDR := os.Getenv(envGatewayCIDR)
	if err := buildBridge(os.Getenv(envBridge), gwCIDR); err != nil {
		writeReply(out, reply{Error: err.Error()})
		os.Exit(1)
	}

	// Start the space DNS resolver on the bridge gateway, inside this netns,
	// so attached containers resolve sibling services by name (and forward
	// external names to the host's upstream resolvers).
	names := newNameTable()
	if err := startSpaceDNS(capNS, names, gwCIDR); err != nil {
		writeReply(out, reply{Error: err.Error()})
		os.Exit(1)
	}
	reqs := make(chan workerReq)
	attachListener, err := listenAttachServer(reqs)
	if err != nil {
		writeReply(out, reply{Error: err.Error()})
		os.Exit(1)
	}
	if attachListener != nil {
		defer attachListener.Close()
		defer os.Remove(os.Getenv(envAttachSock))
	}
	writeReply(out, reply{OK: true})

	// Serve host ingress on the inherited control socket (fd-passing of accepted
	// client sockets), if the host wired one. Best-effort: a bad/absent fd only
	// disables host ingress; east-west networking is untouched.
	if os.Getenv(ingressEnv) == "1" {
		startIngressServer(capNS, names)
	}

	// Also serve ingress on the FS socket so a separate host process (the gateway
	// mux) can fd-pass connections into this netns. Best-effort: a listen failure
	// only disables the mux path; the inherited-fd path is unaffected.
	if ingressListener, ierr := listenIngressFS(capNS, names); ierr != nil {
		fmt.Fprintf(os.Stderr, "spacenet worker: ingress FS socket: %v\n", ierr)
	} else if ingressListener != nil {
		defer ingressListener.Close()
		defer os.Remove(os.Getenv(envIngressSock))
	}

	// Start the native L7 ingress (a goroutine of this worker, not a container):
	// a reverse proxy that resolves backends by space DNS and serves path/TLS
	// routes the mux fd-passes here. The host pushes its table via l7routes
	// commands. Best-effort: a nil handler just means no L7 routing.
	l7 := startL7Ingress(capNS, names, gatewayIPFromCIDR(gwCIDR))

	// Unified work queue: the stdin reader and the attach socket both push onto
	// reqs; this thread is the only one that touches the space netns.
	go readStdinCommands(reqs)

	for req := range reqs {
		switch {
		case req.stop:
			return
		case req.attach != nil:
			req.resp <- runHookAttach(names, req.attach)
		case req.stdin != nil:
			if req.stdin.Op == "close" {
				req.resp <- reply{OK: true}
				return
			}
			req.resp <- dispatch(capNS, names, l7, *req.stdin)
		}
	}
}

// readStdinCommands reads line-delimited JSON commands from the host pipe and
// forwards each onto reqs (with a reply channel it drains and writes back to
// stdout), so stdin commands run on the same pinned thread as attach requests.
// It returns when stdin closes (host gone); the worker then exits and tears the
// netns down.
func readStdinCommands(reqs chan<- workerReq) {
	out := bufio.NewWriter(os.Stdout)
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		var cmd command
		if err := json.Unmarshal(in.Bytes(), &cmd); err != nil {
			writeReply(out, reply{Error: fmt.Sprintf("decode command: %v", err)})
			continue
		}
		resp := make(chan reply, 1)
		reqs <- workerReq{stdin: &cmd, resp: resp}
		r := <-resp
		writeReply(out, r)
		if cmd.Op == "close" {
			return
		}
	}
	reqs <- workerReq{stop: true}
}

// startSpaceDNS launches the resolver on the gateway address inside the
// space netns. Binding happens on a thread pinned to capNS (a fresh
// goroutine could otherwise be scheduled on a host-netns thread), so the
// listeners stay in the space netns for the worker's lifetime. It returns
// once the sockets are bound (or with the bind error), so readiness is honest.
func startSpaceDNS(capNS netns.NsHandle, names *nameTable, gwCIDR string) error {
	gw, _, err := net.ParseCIDR(gwCIDR)
	if err != nil {
		return fmt.Errorf("dns: parse gateway %q: %w", gwCIDR, err)
	}
	upstream, ctrl := hostUpstreams()

	type bindResult struct{ err error }
	done := make(chan bindResult, 1)
	go func() {
		if err := enterSpaceNetns(capNS); err != nil {
			done <- bindResult{fmt.Errorf("dns: enter space netns: %w", err)}
			return
		}
		_, err := newSpaceDNS(names, gw, dnsPort, upstream, ctrl)
		done <- bindResult{err}
		// Goroutine stays alive (and OS-thread-locked to capNS) holding the
		// listeners; it never returns so the netns binding is never released.
		select {}
	}()
	return (<-done).err
}

// buildBridge creates the space bridge, assigns the gateway address, and
// brings it (and loopback) up — the exact sequence the netspike validated.
func buildBridge(name, gwCIDR string) error {
	// Pin a stable, locally-administered MAC on the bridge. A Linux bridge
	// otherwise borrows the MAC of its lowest-indexed enslaved port; deleting
	// that port on Detach makes the bridge silently change its own MAC, and
	// every container's cached ARP for the gateway then points at a dead MAC
	// until it ages out (~tens of seconds of total connectivity loss). A fixed
	// MAC makes the gateway address immutable across port churn. Containers
	// derive it deterministically from the bridge name (02:* = locally admin).
	br := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: name, HardwareAddr: bridgeMAC(name)}}
	if err := netlink.LinkAdd(br); err != nil {
		return fmt.Errorf("bridge %q add: %w", name, err)
	}
	addr, err := netlink.ParseAddr(gwCIDR)
	if err != nil {
		return fmt.Errorf("parse gateway %q: %w", gwCIDR, err)
	}
	if err := netlink.AddrAdd(br, addr); err != nil {
		return fmt.Errorf("bridge %q addr: %w", name, err)
	}
	if err := netlink.LinkSetUp(br); err != nil {
		return fmt.Errorf("bridge %q up: %w", name, err)
	}
	if lo, err := netlink.LinkByName("lo"); err == nil {
		_ = netlink.LinkSetUp(lo)
	}
	return nil
}

func dispatch(capNS netns.NsHandle, names *nameTable, l7 *l7Ingress, cmd command) reply {
	switch cmd.Op {
	case "attach":
		if err := doAttach(capNS, cmd); err != nil {
			return reply{Error: err.Error()}
		}
		return reply{OK: true}
	case "l7routes":
		if err := applyL7Routes(l7, cmd.Routes); err != nil {
			return reply{Error: err.Error()}
		}
		return reply{OK: true}
	case "register":
		names.register(cmd.Name, net.ParseIP(cmd.IP))
		return reply{OK: true}
	case "unregister":
		names.unregister(cmd.Name, net.ParseIP(cmd.IP))
		return reply{OK: true}
	case "acl":
		if err := programACL(cmd.IF, cmd.Edges, names); err != nil {
			return reply{Error: err.Error()}
		}
		return reply{OK: true}
	case "detach":
		// Removing the host-side veth in the space netns also drops the
		// peer; if the container netns is already gone this is a no-op.
		if link, err := netlink.LinkByName(cmd.Host); err == nil {
			_ = netlink.LinkDel(link)
		}
		return reply{OK: true}
	case "spawnsim":
		pid, err := spawnNetnsHolder()
		if err != nil {
			return reply{Error: err.Error()}
		}
		return reply{OK: true, PID: pid}
	case "spawnoverlay":
		var child *childUserns
		if cmd.ChildRoot > 0 {
			child = &childUserns{RootID: cmd.ChildRoot, Count: cmd.ChildCount}
			addChildRootUID(cmd.ChildRoot)
			// The service's createRuntime hook connects as the child root: the
			// socket must be its to write, and nobody else's.
			if sock := os.Getenv(envAttachSock); sock != "" {
				if err := os.Chown(sock, cmd.ChildRoot, cmd.ChildRoot); err == nil {
					_ = os.Chmod(sock, 0o600)
				}
				if cmd.LockSockDir {
					if err := lockSockDir(filepath.Dir(sock), cmd.ChildRoot); err != nil {
						return reply{Error: err.Error()}
					}
				}
			}
		}
		pid, err := spawnOverlayHolder(cmd.Lower, cmd.Upper, cmd.Work, cmd.Merged, child)
		if err != nil {
			return reply{Error: err.Error()}
		}
		return reply{OK: true, PID: pid}
	case "egress":
		// Route the bridge subnet (cmd.CIDR) out of the slirp tap (cmd.IF):
		// forwarding + MASQUERADE, inside this space netns.
		if err := programEgress(cmd.CIDR, cmd.IF, cmd.AllowHost, cmd.AllowEgress); err != nil {
			return reply{Error: err.Error()}
		}
		return reply{OK: true}
	case "hostguard":
		if err := programHostGuard(cmd.IF, cmd.HostAddrs, cmd.ControllerAddrs, cmd.Ports, cmd.PublicAddrs); err != nil {
			return reply{Error: err.Error()}
		}
		return reply{OK: true}
	case "egressStatus":
		return egressStatus(cmd.IF, cmd.AllowHost)
	case "peerBlackhole":
		// Report whether the IMDS/RFC1918 deny routes are present INSIDE the
		// peer's own netns (the container side), so a test can assert the
		// source-side reject without entering the namespace itself.
		ok, err := peerBlackhole(capNS, cmd.PID)
		if err != nil {
			return reply{Error: err.Error()}
		}
		return reply{OK: true, Blackhole: ok}
	case "dial":
		if err := doDial(capNS, cmd.PID, cmd.Dst); err != nil {
			return reply{Error: err.Error()}
		}
		return reply{OK: true}
	case "probeTCP":
		// Readiness dial from the space netns to a container's bridge IP. A
		// failed connect is not an error to surface — it is a not-ready signal.
		return reply{OK: doProbeTCP(capNS, cmd.Dst)}
	case "probeHTTP":
		return reply{OK: doProbeHTTP(capNS, cmd.Dst)}
	case "ping":
		if err := doPing(capNS, cmd.PID, cmd.Dst); err != nil {
			return reply{Error: err.Error()}
		}
		return reply{OK: true}
	case "resolve":
		ips, err := doResolve(capNS, cmd.PID, cmd.Dst, cmd.Name)
		if err != nil {
			return reply{Error: err.Error()}
		}
		return reply{OK: true, IPs: ips}
	default:
		return reply{Error: fmt.Sprintf("unknown op %q", cmd.Op)}
	}
}

// doAttach implements the legacy post-start attach: move the veth into the
// container netns, reopen that netns, then configure the peer from inside it.
// The OCI-hook path splits those two phases because a createRuntime hook can
// move the peer but rootless procfs policy may still forbid reopening the
// container netns until later in container init.
func doAttach(capNS netns.NsHandle, cmd command) (err error) {
	if err := doAttachMove(cmd); err != nil {
		return err
	}
	// A configuration failure must not leak the moved peer: deleting the host
	// side removes the pair even after its peer entered the container netns.
	defer func() {
		if err != nil {
			if l, e := netlink.LinkByName(cmd.Host); e == nil {
				_ = netlink.LinkDel(l)
			}
			_ = netns.Set(capNS)
		}
	}()

	// Configure the peer inside the container netns, then come home.
	ctrNS, err := netns.GetFromPid(cmd.PID)
	if err != nil {
		return fmt.Errorf("open container netns (pid %d): %w", cmd.PID, err)
	}
	defer ctrNS.Close()
	if err := netns.Set(ctrNS); err != nil {
		return fmt.Errorf("enter container netns: %w", err)
	}
	cfgErr := configurePeer(cmd)
	if setErr := netns.Set(capNS); setErr != nil && cfgErr == nil {
		cfgErr = fmt.Errorf("return to space netns: %w", setErr)
	}
	return cfgErr
}

// doAttachMove performs the createRuntime-safe half of attach: create the veth
// in the space netns and move its peer into the container netns. It does not
// reopen /proc/<pid>/ns/net; createContainer configures the peer from inside
// that namespace before the service command execs.
func doAttachMove(cmd command) (err error) {
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: cmd.Host},
		PeerName:  cmd.Peer,
	}
	if err := netlink.LinkAdd(veth); err != nil {
		return fmt.Errorf("veth %s<->%s add: %w", cmd.Host, cmd.Peer, err)
	}
	// From here a failure must not leak the half-built veth.
	defer func() {
		if err != nil {
			if l, e := netlink.LinkByName(cmd.Host); e == nil {
				_ = netlink.LinkDel(l)
			}
		}
	}()

	br, err := netlink.LinkByName(os.Getenv(envBridge))
	if err != nil {
		return fmt.Errorf("lookup bridge: %w", err)
	}
	if err := netlink.LinkSetMaster(veth, br.(*netlink.Bridge)); err != nil {
		return fmt.Errorf("enslave %s to bridge: %w", cmd.Host, err)
	}
	if err := netlink.LinkSetUp(veth); err != nil {
		return fmt.Errorf("host side %s up: %w", cmd.Host, err)
	}

	peer, err := netlink.LinkByName(cmd.Peer)
	if err != nil {
		return fmt.Errorf("lookup peer %s: %w", cmd.Peer, err)
	}
	// The contested op. EPERM here means the container did NOT join the
	// space userns (see package doc); surface it verbatim.
	if err := netlink.LinkSetNsPid(peer, cmd.PID); err != nil {
		return fmt.Errorf("move peer into container netns (pid %d): %w "+
			"(container must share the space user namespace)", cmd.PID, err)
	}
	return nil
}

// configurePeer runs inside the container netns: rename the peer to the
// requested interface (eth0), give it the allocated address, bring it and
// loopback up, and add the default route via the space gateway.
func configurePeer(cmd command) error {
	peer, err := netlink.LinkByName(cmd.Peer)
	if err != nil {
		return fmt.Errorf("peer %s in container: %w", cmd.Peer, err)
	}
	if err := netlink.LinkSetName(peer, cmd.IF); err != nil {
		return fmt.Errorf("rename %s->%s: %w", cmd.Peer, cmd.IF, err)
	}
	iface, err := netlink.LinkByName(cmd.IF)
	if err != nil {
		return fmt.Errorf("lookup %s: %w", cmd.IF, err)
	}
	addr, err := netlink.ParseAddr(cmd.CIDR)
	if err != nil {
		return fmt.Errorf("parse %s: %w", cmd.CIDR, err)
	}
	if err := netlink.AddrAdd(iface, addr); err != nil {
		return fmt.Errorf("addr %s on %s: %w", cmd.CIDR, cmd.IF, err)
	}
	if err := netlink.LinkSetUp(iface); err != nil {
		return fmt.Errorf("%s up: %w", cmd.IF, err)
	}
	if lo, err := netlink.LinkByName("lo"); err == nil {
		_ = netlink.LinkSetUp(lo)
	}
	gw := net.ParseIP(cmd.GwIP)
	if gw == nil {
		return fmt.Errorf("parse gateway ip %q", cmd.GwIP)
	}
	if err := netlink.RouteAdd(&netlink.Route{
		LinkIndex: iface.Attrs().Index,
		Gw:        gw,
	}); err != nil {
		return fmt.Errorf("default route via %s: %w", cmd.GwIP, err)
	}
	// Deny IMDS/RFC1918 from INSIDE the container netns: a blackhole here rejects
	// at the source (immediate "network unreachable") instead of relying on the
	// worker-side blackhole catching the packet as forwarded transit (silent
	// drop / timeout). The container's eth0 /24 is connected (more specific), so
	// LPM keeps east-west + the default route alive; only IMDS and the host LAN,
	// which have no more-specific route, hit the blackhole. Best-effort.
	blackholeMeta()
	// Re-open the SAME /32 exceptions EnableEgress already punched in the
	// space's bridge netns (allowHost LAN addrs, wgAllow's mesh subnet, the
	// Network Controller's address) — this blackhole is a SEPARATE one, freshly
	// applied inside the container's own netns, so without this the container
	// would be unable to reach anything the bridge netns was explicitly allowed
	// to. Routed via the SAME gateway as the default route (cmd.GwIP): the
	// bridge, one hop away, already knows how to forward it onward (egress.go's
	// own installHostAllowRoutes call did the equivalent for the bridge itself).
	if len(cmd.AllowHost) > 0 {
		installHostAllowRoutes(cmd.AllowHost, gw, iface.Attrs().Index)
	}
	return nil
}

func writeReply(w *bufio.Writer, r reply) {
	b, _ := json.Marshal(r)
	w.Write(b)
	w.WriteByte('\n')
	w.Flush()
}

// supervisorGone reports whether the process that spawned this worker is no
// longer its parent: the worker was reparented (to pid 1 or a subreaper) when
// the supervisor died during the stage1/execve window.
func supervisorGone() bool {
	want, err := strconv.Atoi(os.Getenv(envSupervisorPID))
	if err != nil || want <= 0 {
		return os.Getppid() == 1
	}
	return os.Getppid() != want
}

// lockSockDir keeps the attach socket's directory reachable by the operator
// (its owner) and the child root (its group, traverse only) and nobody else:
// the guarantee the 0700 runtime dir gives the shared-userns sockets, with
// one more uid let in. Other spaces' roots stop at the door.
func lockSockDir(dir string, childRoot int) error {
	if err := os.Chown(dir, -1, childRoot); err != nil {
		return fmt.Errorf("attach socket dir %q: group to child root: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o710); err != nil {
		return fmt.Errorf("attach socket dir %q: mode 0710: %w", dir, err)
	}
	return nil
}
