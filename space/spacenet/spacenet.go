//go:build linux

package spacenet

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/lalternativefabrique/sklp/space/ociproc"
)

// SpaceNet is one space's rootless network: a bridge in a detached
// user+net namespace, plus the IPAM and the worker process that owns those
// namespaces. It is safe for concurrent Attach/Detach calls. The zero value
// is not usable; build one with NewSpaceNet.
type SpaceNet struct {
	name   string
	bridge string
	ipam   *ipam

	worker *exec.Cmd
	// identity is the mapping the worker's userns carries; every service joins
	// that userns, so it is who a service container is on the host.
	identity ociproc.Identity
	mu       sync.Mutex // serialises the worker request/response pipe
	stdin    io.WriteCloser
	stdout   *bufio.Reader

	// ctrl is the host end of the SEQPACKET socketpair the worker serves ingress
	// on (fd-passing of accepted client sockets). ctrlMu serialises sends since a
	// SCM_RIGHTS write must be one atomic message. Both nil when ingress is off.
	ctrl   *net.UnixConn
	ctrlMu sync.Mutex

	vethSeq  int                // monotonic suffix for unique veth names
	attached map[int]attachment // container pid -> its veth + IP (for detach)
	closed   bool

	// stopEgress kills the slirp4netns attached by EnableEgress. Owned here so
	// Close always reaps it even if the caller forgets the stop func it was
	// handed — slirp lives on the HOST stack (not the worker netns), so killing
	// the worker does NOT take slirp down; only this kill does.
	stopEgress func()

	// allowHostAddrs are the host /32s EnableEgress punched through the RFC1918
	// blackhole (`sklp dev` allowHost; nil everywhere else). Kept so EgressStatus
	// can assert the exception is still in force.
	allowHostAddrs []string

	// hostGuard is on for every space whose egress is up outside `sklp dev`:
	// the addresses it keeps containers off, and the controller addresses it
	// lets them reach on the controller's ports.
	hostGuard            bool
	guardHostAddrs       []string
	guardControllerAddrs []string
	guardPublicAddrs     []string

	resolvConf string // path to the generated resolv.conf (lazily created)
	hostsFile  string // path to the generated /etc/hosts (lazily created)

	// attachSock is the filesystem path of the worker's attach control socket
	// (the createRuntime hook connects to it to attach a container BEFORE its
	// command execs). Empty if the socket could not be wired — the launcher then
	// falls back to the post-start host-side Attach.
	attachSock string
	// attachSockDirOwned is set when the caller chose the socket's directory
	// (NewSpaceNetAt): the worker then locks that directory down to the
	// operator and the child root, the way the 0700 runtime dir is locked
	// down to the operator alone.
	attachSockDirOwned bool

	// ingressSock is the filesystem path of the worker's ingress control socket
	// (unixpacket). The gateway mux — a SEPARATE host process — dials it to
	// fd-pass accepted client connections into this space's netns. Empty if the
	// host tmp dir was unusable (the mux then has no way in for this space).
	ingressSock string
}

// attachment records what a single Attach allocated so Detach can undo it:
// the host-side veth to delete, the IP to return to the pool, and the service
// name to drop from the DNS table.
type attachment struct {
	host string
	ip   net.IP
	name string
}

// Identity is the mapping the space userns carries: who a service container
// of this space is on the host.
func (c *SpaceNet) Identity() ociproc.Identity { return c.identity }

// WorkerPID is the worker's host pid: /proc/<pid>/ns/user is the space user
// namespace, whose root is the operator with authority over every child.
func (c *SpaceNet) WorkerPID() int { return c.worker.Process.Pid }

// Endpoints are the kernel-stable namespace handles a caller threads into a
// container's OCI spec so the container shares the space user namespace and
// can be attached. NetnsPath is the space bridge netns — NOT what a
// container joins (a container keeps its OWN netns); UsernsPath is the one a
// container MUST join (see package doc).
type Endpoints struct {
	UsernsPath string // /proc/<worker>/ns/user — containers MUST join this
	NetnsPath  string // /proc/<worker>/ns/net  — the space bridge netns
	Gateway    string // bridge gateway IP, e.g. 10.88.0.1
}

// newSpaceNetMu serialises NewSpaceNet across every space on the host — see
// its doc for why. Package-level (not per-name): the race it closes is
// BETWEEN spaces, so scoping the lock to one space's name would not help.
var newSpaceNetMu sync.Mutex

// NewSpaceNet creates (or rebuilds) the space network named name on the
// given subnet (CIDR, e.g. "10.88.0.0/24"). It re-execs the host binary into
// a fresh user+net namespace where it owns CAP_NET_ADMIN, has it build the
// bridge with the gateway address (subnet's .1), and keeps it parked so the
// namespaces persist for the space's lifetime. The host binary MUST call
// MaybeRunWorker early in main() for the re-exec to take effect.
//
// The whole function runs under newSpaceNetMu, serialising it across every
// space on the host. This closes a real race hit when two spaces on one
// multi-space node (see sklet agent's RunAgent, sklet/agent.go)
// call NewSpaceNet concurrently: reapOrphanWorkers (called at the top of
// EVERY call, to sweep a prior space's worker leaked by a kill -9) scans
// /proc for any process carrying the worker env markers whose ppid is 1,
// and SIGKILLs it. A worker mid-clone (this function, below) can appear
// with ppid==1 for a brief kernel-timing window before the parent
// relationship and Pdeathsig are fully established — exactly matching a
// genuine orphan from the reaper's point of view. Without serialisation, a
// SECOND space's reapOrphanWorkers can land in that window and kill the
// FIRST space's just-forked worker, which then fails downstream (observed
// as newuidmap's "waitid: no child processes" — the worker was already
// reaped out from under it) with NO further retry: the failure is
// indistinguishable from a real config problem to the caller, and (before
// this fix) spaceNet's cached-error TTL was the only thing standing between
// this and a permanently wedged space. Serialising the whole
// reap-through-ready sequence means no other space's reaper can ever run
// while this one has a worker in its vulnerable window. The cost is spaces
// on one host starting their networks one at a time instead of in parallel
// — milliseconds, not a user-visible latency concern, and unconditionally
// safer than leaving the race in place.
func NewSpaceNet(name, subnet string) (*SpaceNet, error) {
	return NewSpaceNetAt(name, subnet, "")
}

// NewSpaceNetAt is NewSpaceNet with the attach control socket at attachSock
// instead of the 0700 runtime dir. A space whose services run as a child
// root needs the socket somewhere that root can reach; who may connect is
// still decided by peer credentials (see checkPeerUID).
func NewSpaceNetAt(name, subnet, attachSock string) (*SpaceNet, error) {
	// Serialises this whole function across every concurrent NewSpaceNet call
	// on the host (all spaces, not just this one) — see newSpaceNetMu's doc.
	newSpaceNetMu.Lock()
	defer newSpaceNetMu.Unlock()

	// Reap anything a prior space leaked by dying without Close (crash / kill -9).
	// Workers FIRST: a worker forked before its Pdeathsig was armed can be
	// orphaned (reparented to PID 1); killing it destroys its netns + bridge.
	// Then slirps, which run on the host stack and outlive their worker — and any
	// worker we just killed now reads as a dead netns target, so this second pass
	// sweeps its slirp too. Both best-effort; neither touches a still-owned space.
	reapOrphanWorkers()
	reapOrphanSlirp()

	alloc, err := newIPAM(subnet)
	if err != nil {
		return nil, err
	}
	bridge := bridgeName(name)

	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("spacenet: resolve self: %w", err)
	}
	// Ingress control channel: a SEQPACKET socketpair. The worker end is passed
	// as the first ExtraFile (lands at ingressFD in the child) so the worker can
	// receive accepted client sockets (SCM_RIGHTS) and splice them to a backend
	// dialed in the space netns. Best-effort: if we cannot build it, the
	// space still works east-west — only host ingress is unavailable.
	hostCtrl, workerCtrlFile := newIngressPair()

	// Attach control socket: the createRuntime hook connects here to attach a
	// container's veth BEFORE its command execs (closing the oneshot race). The
	// path is space-private under the host tmp dir; worker and hook share the
	// host mount namespace, so a filesystem path reaches both. Best-effort — a
	// path build failure leaves attachSock empty and the launcher falls back to
	// the post-start host-side Attach.
	attachSockDirOwned := attachSock != ""
	if attachSock == "" {
		attachSock = attachSockPath(name)
	}

	// Ingress control socket on the filesystem: lets a SEPARATE host process (the
	// gateway mux) fd-pass accepted client connections into this space's netns,
	// which the inherited fd-pair cannot do across process boundaries. Same
	// space-private host-tmp path trick as attachSock.
	ingressSock := ingressSockPath(name)

	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(),
		envBridge+"="+bridge,
		envGatewayCIDR+"="+alloc.gatewayCIDR(),
	)
	if attachSock != "" {
		cmd.Env = append(cmd.Env, envAttachSock+"="+attachSock)
	}
	if ingressSock != "" {
		cmd.Env = append(cmd.Env, envIngressSock+"="+ingressSock)
	}
	cmd.Stderr = os.Stderr // worker netlink failures surface to the host log

	// ExtraFiles map sequentially onto fds starting at 3. Ingress, when wired,
	// always takes the first slot (fd ingressFD=3); the subuid sync pipe, when
	// used, takes the next. So the worker never hard-codes its sync fd — we pass
	// the number via env (envSyncFD) and the worker reads it from there.
	extra := []*os.File{}
	if workerCtrlFile != nil {
		extra = append(extra, workerCtrlFile)
		cmd.Env = append(cmd.Env, ingressEnv+"=1")
	}

	// Decide the userns mapping strategy. With newuidmap/newgidmap present we
	// clone an EMPTY userns and write the FULL subuid range from the parent
	// after start (see below): container uid 0 -> our uid, AND uid 1..65536 ->
	// 100000.. . That range is what lets a JOINING container (nginx, postgres)
	// chown to a non-root uid like 101 without EINVAL. Without the helpers we
	// fall back to the single-id map the Go runtime can write itself.
	cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%d", envSupervisorPID, os.Getpid()))
	tools, useRange := lookupSubidTools()
	identity := ociproc.SingleIdentity(os.Getuid(), os.Getgid())
	if useRange {
		id, rerr := resolveIdentity(os.Getuid(), os.Getgid())
		if rerr != nil {
			if hostCtrl != nil {
				hostCtrl.Close()
			}
			return nil, fmt.Errorf("spacenet: map subuid range: %w", rerr)
		}
		identity = id
	}
	// Pdeathsig is the only teardown defence that fires on a SIGKILL of the
	// supervisor: a SIGKILL'd process runs no handler, so it cannot signal a
	// process group — only the kernel, on parent death, reaps the worker for us.
	// Killing the worker destroys its netns, taking the bridge + every veth with
	// it. Setpgid scopes the group for clean (Close) teardown. The startup GC
	// stays the backstop for the gap between fork and the maps being wired.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
		Pdeathsig:  syscall.SIGKILL,
		Setpgid:    true,
	}
	if useRange {
		// Stage 1 waits for the map, then re-execs into stage 2 (caps land at the
		// execve boundary); startMapped wires the sync pipe and writes the maps.
		cmd.Env = append(cmd.Env, workerEnv+"=1")
	} else {
		// Fallback: the Go runtime writes a single-id map directly at clone time,
		// so the kernel grants caps immediately — go straight to stage 2. Only the
		// single root id is mapped: non-root-uid images (e.g. nginx -> chown 101)
		// may fail to chown in this space.
		fmt.Fprintln(os.Stderr, "spacenet: subuid range unavailable (newuidmap/newgidmap or an /etc/subuid entry missing); "+
			"non-root-uid images (nginx) may fail to chown in a space")
		cmd.Env = append(cmd.Env, workerEnv+"=2")
		cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}
		cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}
	}
	if len(extra) > 0 {
		cmd.ExtraFiles = extra
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cleanupWorkerFiles(hostCtrl)
		return nil, fmt.Errorf("spacenet: worker stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cleanupWorkerFiles(hostCtrl)
		return nil, fmt.Errorf("spacenet: worker stdout: %w", err)
	}
	// Both paths start pinned: Pdeathsig is armed by the pinned thread (see
	// startPinned). The range path also parks the worker until its maps are in.
	if useRange {
		err = startMapped(cmd, func(pid int) error {
			return tools.applySubidRange(pid, identity)
		})
	} else {
		err = startPinned(cmd)
	}
	if err != nil {
		cleanupWorkerFiles(hostCtrl)
		return nil, fmt.Errorf("spacenet: start worker: %w", err)
	}
	// The worker has its own copies now; drop the host's copies of the ends we
	// passed via ExtraFiles.
	if workerCtrlFile != nil {
		_ = workerCtrlFile.Close()
	}
	for _, f := range extra {
		_ = f.Close() // each ExtraFile was dup'd into the child at fork
	}

	c := &SpaceNet{
		name:       name,
		bridge:     bridge,
		ipam:       alloc,
		worker:     cmd,
		identity:   identity,
		stdin:      stdin,
		stdout:     bufio.NewReader(stdout),
		ctrl:       hostCtrl,
		attached:   map[int]attachment{},
		attachSock: attachSock,

		attachSockDirOwned: attachSockDirOwned,
		ingressSock:        ingressSock,
	}

	// The worker builds the bridge before reading any command; if it failed
	// it has already exited. Probe with a no-op-ish wait on its ns handle.
	if err := c.waitWorkerReady(); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// cleanupWorkerFiles closes the host-side fds we would otherwise leak when the
// worker never starts (the worker's ExtraFile copies are still ours until the
// child forks).
func cleanupWorkerFiles(hostCtrl *net.UnixConn) {
	if hostCtrl != nil {
		hostCtrl.Close()
	}
}

// Endpoints returns the namespace handles to thread into container specs.
func (c *SpaceNet) Endpoints() Endpoints {
	pid := c.worker.Process.Pid
	return Endpoints{
		UsernsPath: fmt.Sprintf("/proc/%d/ns/user", pid),
		NetnsPath:  fmt.Sprintf("/proc/%d/ns/net", pid),
		Gateway:    c.ipam.gateway.String(),
	}
}

// ResolvConfPath returns the path to a generated resolv.conf whose sole
// nameserver is the space gateway (where the resolver listens). Callers
// bind-mount it at /etc/resolv.conf in the container's OCI spec so the
// container resolves sibling service names and forwarded external names
// through the space DNS. The file is created once on first call and lives
// for the space's lifetime; the OS reaps it on the next /tmp sweep after
// teardown (no cleanup hook needed for a space-scoped temp file).
func (c *SpaceNet) ResolvConfPath() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resolvConf != "" {
		return c.resolvConf, nil
	}
	f, err := os.CreateTemp("", "space-resolv-"+short(c.name)+"-*.conf")
	if err != nil {
		return "", fmt.Errorf("spacenet: create resolv.conf: %w", err)
	}
	defer f.Close()
	// CreateTemp is 0600; bind-mounted at /etc/resolv.conf it must be world-
	// readable, else a non-root service (e.g. postgres uid 999) can't read it
	// and every DNS lookup fails with "Temporary failure in name resolution".
	if err := f.Chmod(0o644); err != nil {
		return "", fmt.Errorf("spacenet: chmod resolv.conf: %w", err)
	}
	// options ndots:0 so a bare "db" is tried as-is before any search suffix.
	content := fmt.Sprintf("nameserver %s\noptions ndots:0\n", c.ipam.gateway.String())
	if _, err := f.WriteString(content); err != nil {
		return "", fmt.Errorf("spacenet: write resolv.conf: %w", err)
	}
	c.resolvConf = f.Name()
	return c.resolvConf, nil
}

// HostsPath returns the path to a generated /etc/hosts callers bind-mount at
// /etc/hosts in the container's OCI spec. A space netns ships no /etc/hosts
// (unlike docker, which always injects one), so without this an image that
// resolves localhost/loopback/0.0.0.0 to announce itself (e.g. nats) aborts on
// boot with "address can not be resolved". The minimal loopback entries fix
// that; the gateway line lets the container self-resolve the spec hostname.
// File created once on first call, lives for the space's lifetime (OS reaps
// it on the next /tmp sweep after teardown, same as ResolvConfPath).
func (c *SpaceNet) HostsPath() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hostsFile != "" {
		return c.hostsFile, nil
	}
	f, err := os.CreateTemp("", "space-hosts-"+short(c.name)+"-*")
	if err != nil {
		return "", fmt.Errorf("spacenet: create hosts: %w", err)
	}
	defer f.Close()
	// World-readable like resolv.conf above: bind-mounted at /etc/hosts it must
	// be readable by non-root services, else loopback resolution breaks for them.
	if err := f.Chmod(0o644); err != nil {
		return "", fmt.Errorf("spacenet: chmod hosts: %w", err)
	}
	// The spec hostname "sklp" resolves to the container itself, on
	// 127.0.1.1 as Debian does: this file is shared by every container of the
	// space, so it cannot carry each one's own address. Pointed at the bridge
	// gateway, an Erlang node (RabbitMQ) looked for its own epmd on the host
	// side and never booted.
	content := "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n127.0.1.1\tsklp\n"
	if _, err := f.WriteString(content); err != nil {
		return "", fmt.Errorf("spacenet: write hosts: %w", err)
	}
	c.hostsFile = f.Name()
	return c.hostsFile, nil
}

// Attach wires the container whose init PID is containerPID onto the space
// bridge: it allocates an IP, builds a veth pair, enslaves the host end to the
// bridge, pushes the peer into the container netns, renames it to ifName,
// addresses it, and routes its default gateway to the bridge. The service name
// (e.g. "db") is registered in the space DNS so siblings resolve it to this
// IP; multiple replicas may share one name (round-robin DNS). An empty name
// skips DNS registration. It returns the assigned IP (without prefix).
//
// REQUIREMENT: containerPID must run in the space user namespace
// (Endpoints().UsernsPath). A container with its own userns is rejected by the
// kernel with EPERM, surfaced here verbatim — there is no rootless workaround.
func (c *SpaceNet) Attach(name string, containerPID int, ifName string) (string, error) {
	if ifName == "" {
		ifName = "eth0"
	}
	cidr, ip, err := c.ipam.allocate()
	if err != nil {
		return "", err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		c.ipam.release(ip)
		return "", fmt.Errorf("spacenet: %q is closed", c.name)
	}
	c.vethSeq++
	// "ve" + frag(<=5) + seq(<=4) + side(1) <= 12 bytes, under IFNAMSIZ-1.
	frag := nameFrag(c.name, 5)
	host := fmt.Sprintf("ve%s%dh", frag, c.vethSeq)
	peer := fmt.Sprintf("ve%s%dp", frag, c.vethSeq)

	rep, err := c.send(command{
		Op:        "attach",
		PID:       containerPID,
		IF:        ifName,
		CIDR:      cidr,
		Host:      host,
		Peer:      peer,
		GwIP:      c.ipam.gateway.String(),
		AllowHost: c.allowHostAddrs,
	})
	if err != nil || !rep.OK {
		c.ipam.release(ip)
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("spacenet: attach pid %d: %s", containerPID, rep.Error)
	}
	c.attached[containerPID] = attachment{host: host, ip: ip, name: name}

	// Register the name -> IP in the worker's DNS table. A failure here does
	// not unwind the (working) attach; the IP is reachable, only its name is.
	if name != "" {
		if rep, err := c.send(command{Op: "register", Name: name, IP: ip.String()}); err != nil {
			return "", err
		} else if !rep.OK {
			return "", fmt.Errorf("spacenet: register %q: %s", name, rep.Error)
		}
	}
	return ip.String(), nil
}

// SpawnPeer forks a parked process inside the space's user namespace with
// its own network namespace, returning its host-visible pid. The pid is then
// a valid Attach target (it shares the space userns, so the veth push is
// permitted). It models a runc container that joins the space userns while
// keeping a private netns, and lets the network be exercised without runc.
// The peer is killed when the space worker exits (Pdeathsig).
func (c *SpaceNet) SpawnPeer() (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, fmt.Errorf("spacenet: %q is closed", c.name)
	}
	rep, err := c.send(command{Op: "spawnsim"})
	if err != nil {
		return 0, err
	}
	if !rep.OK {
		return 0, fmt.Errorf("spacenet: spawn peer: %s", rep.Error)
	}
	return rep.PID, nil
}

// SetL7Routes pushes the native L7 ingress routing table (JSON-encoded
// []ingress.Route) to the worker, which hot-swaps it into the in-netns reverse
// proxy. Empty routesJSON clears the table. It is the host edge of the worker's
// l7routes command — the deploy agent calls it when a space's path/TLS routes
// change, the analogue of writing the traefik dyn dir, but for the native
// in-worker ingress.
func (c *SpaceNet) SetL7Routes(routesJSON string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("spacenet: %q is closed", c.name)
	}
	rep, err := c.send(command{Op: "l7routes", Routes: routesJSON})
	if err != nil {
		return err
	}
	if !rep.OK {
		return fmt.Errorf("spacenet: set l7 routes: %s", rep.Error)
	}
	return nil
}

// SpawnOverlay forks a parked holder inside the space user namespace with its
// OWN mount namespace, in which it mounts a per-service overlayfs (lower = the
// shared read-only digest rootfs, upper = per-service scratch). It returns the
// holder's host-visible pid; sklp then `nsenter -U --mount`s runc into it so the
// container's rootfs is the writable merged overlay while the digest lower stays
// pristine and shared. The holder dies with the worker (Pdeathsig), unmounting
// the overlay. lower/upper/work/merged must already exist on disk.
func (c *SpaceNet) SpawnOverlay(lower, upper, work, merged string) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, fmt.Errorf("spacenet: %q is closed", c.name)
	}
	rep, err := c.send(command{Op: "spawnoverlay", Lower: lower, Upper: upper, Work: work, Merged: merged})
	if err != nil {
		return 0, err
	}
	if !rep.OK {
		return 0, fmt.Errorf("spacenet: spawn overlay: %s", rep.Error)
	}
	return rep.PID, nil
}

// SpawnOverlayChild is SpawnOverlay with the holder in a CHILD user namespace
// of the space's: uid 0 there is the id slot places from the top of the
// delegated range, a host uid that owns nothing, while ids 1.. below it map
// exactly as they do in the space. Each space of a node takes its own slot,
// so two spaces never share a root. runc entered into that holder (EnterArgs)
// runs the service as that root. The returned Identity is who the service is
// on the host.
func (c *SpaceNet) SpawnOverlayChild(lower, upper, work, merged string, slot int) (int, ociproc.Identity, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, ociproc.Identity{}, fmt.Errorf("spacenet: %q is closed", c.name)
	}
	id := c.identity
	root, ok := id.SpaceRootOf(slot, ChildRootSlots)
	if !ok {
		return 0, ociproc.Identity{}, fmt.Errorf("spacenet: child userns slot %d does not fit the delegated range of %q (%d ids)", slot, c.name, min(id.UIDCount, id.GIDCount))
	}
	child := ociproc.Identity{
		HostUID: root.HostUID, HostGID: root.HostGID,
		UIDBase: id.UIDBase, GIDBase: id.GIDBase,
		UIDCount: root.ChildCount, GIDCount: root.ChildCount,
	}
	rep, err := c.send(command{Op: "spawnoverlay", Lower: lower, Upper: upper, Work: work, Merged: merged, ChildRoot: root.WorkerID, ChildCount: root.ChildCount, LockSockDir: c.attachSockDirOwned})
	if err != nil {
		return 0, ociproc.Identity{}, err
	}
	if !rep.OK {
		return 0, ociproc.Identity{}, fmt.Errorf("spacenet: spawn overlay (child userns): %s", rep.Error)
	}
	return rep.PID, child, nil
}

// Ping sends an ICMP echo from the netns of the peer whose init pid is srcPID
// to dstIP, returning nil iff a reply arrives. It proves L3 reachability over
// the space bridge between two attached netns.
func (c *SpaceNet) Ping(srcPID int, dstIP string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("spacenet: %q is closed", c.name)
	}
	rep, err := c.send(command{Op: "ping", PID: srcPID, Dst: dstIP})
	if err != nil {
		return err
	}
	if !rep.OK {
		return fmt.Errorf("spacenet: ping %s from pid %d: %s", dstIP, srcPID, rep.Error)
	}
	return nil
}

// Resolve sends an A-record DNS query for name from the netns of the peer
// whose init pid is srcPID to the space resolver on the gateway, returning
// the answer IPs. It proves a container resolves space service names (and,
// for external names, exercises the upstream-forward path) over the bridge.
func (c *SpaceNet) Resolve(srcPID int, name string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, fmt.Errorf("spacenet: %q is closed", c.name)
	}
	server := net.JoinHostPort(c.ipam.gateway.String(), "53")
	rep, err := c.send(command{Op: "resolve", PID: srcPID, Dst: server, Name: name})
	if err != nil {
		return nil, err
	}
	if !rep.OK {
		return nil, fmt.Errorf("spacenet: resolve %q from pid %d: %s", name, srcPID, rep.Error)
	}
	return rep.IPs, nil
}

// PeerBlackholeInstalled reports whether the IMDS/RFC1918 deny routes are
// present inside the netns of the peer whose init pid is srcPID — i.e. that the
// source-side SSRF reject configurePeer installs on attach is wired. It exists
// so a test asserts the container-side guard without entering the namespace.
func (c *SpaceNet) PeerBlackholeInstalled(srcPID int) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false, fmt.Errorf("spacenet: %q is closed", c.name)
	}
	rep, err := c.send(command{Op: "peerBlackhole", PID: srcPID})
	if err != nil {
		return false, err
	}
	if !rep.OK {
		return false, fmt.Errorf("spacenet: peer blackhole pid %d: %s", srcPID, rep.Error)
	}
	return rep.Blackhole, nil
}

// Detach removes a container's veth from the bridge (idempotent). The IP is
// returned to the pool. Called when a single container leaves a still-running
// space; full teardown is Close.
func (c *SpaceNet) Detach(containerPID int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.attached[containerPID]
	if !ok || c.closed {
		return nil
	}
	if a.name != "" {
		// Drop the DNS entry first so no client resolves a name to an IP whose
		// veth is about to vanish. Best-effort: detach proceeds regardless.
		_, _ = c.send(command{Op: "unregister", Name: a.name, IP: a.ip.String()})
	}
	rep, err := c.send(command{Op: "detach", Host: a.host})
	delete(c.attached, containerPID)
	c.ipam.release(a.ip) // return the address to the pool even if veth del raced
	if err != nil {
		return err
	}
	if !rep.OK {
		return fmt.Errorf("spacenet: detach pid %d: %s", containerPID, rep.Error)
	}
	return nil
}

// Close tears the space network down: it tells the worker to exit and kills
// it as a backstop. Exiting the worker destroys its net namespace, which
// takes the bridge and every veth with it — no interface or netns leaks on
// the host side. Safe to call more than once.
func (c *SpaceNet) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	trackHostGuard(c, false)
	// Reap the egress slirp first — it runs on the HOST stack, so killing the
	// worker below would orphan it (reparented to PID 1) otherwise. Idempotent
	// via the sync.Once guard, so a caller that already called its stop is fine.
	if c.stopEgress != nil {
		c.stopEgress()
	}
	// Best-effort graceful exit; ignore errors since we kill regardless.
	_, _ = c.send(command{Op: "close"})
	_ = c.stdin.Close()
	if c.ctrl != nil {
		_ = c.ctrl.Close() // unblocks the worker's serveIngress read loop
	}
	c.mu.Unlock()

	if c.worker != nil && c.worker.Process != nil {
		_ = c.worker.Process.Kill()
		_, _ = c.worker.Process.Wait()
	}
	return nil
}

// send writes one command and reads exactly one reply. Callers hold c.mu.
func (c *SpaceNet) send(cmd command) (reply, error) {
	b, err := json.Marshal(cmd)
	if err != nil {
		return reply{}, err
	}
	if _, err := c.stdin.Write(append(b, '\n')); err != nil {
		return reply{}, fmt.Errorf("spacenet: write command: %w", err)
	}
	line, err := c.stdout.ReadBytes('\n')
	if err != nil {
		return reply{}, fmt.Errorf("spacenet: read reply (worker dead?): %w", err)
	}
	var rep reply
	if err := json.Unmarshal(line, &rep); err != nil {
		return reply{}, fmt.Errorf("spacenet: decode reply %q: %w", line, err)
	}
	return rep, nil
}

// waitWorkerReady reads the worker's one-shot readiness reply, emitted after
// it builds the bridge. A non-OK reply (or a closed pipe) means bridge setup
// failed; the worker's stderr carries no extra detail beyond rep.Error.
func (c *SpaceNet) waitWorkerReady() error {
	line, err := c.stdout.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("spacenet: worker for %q never signalled ready: %w", c.name, err)
	}
	var rep reply
	if err := json.Unmarshal(line, &rep); err != nil {
		return fmt.Errorf("spacenet: decode ready reply %q: %w", line, err)
	}
	if !rep.OK {
		return fmt.Errorf("spacenet: worker for %q failed to start: %s", c.name, rep.Error)
	}
	return nil
}
