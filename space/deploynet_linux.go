//go:build linux

package space

import (
	"context"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lalternativefabrique/sklp/space/ociproc"
	"github.com/lalternativefabrique/sklp/space/spacenet"
)

// attachPlan wraps a spacenet.AttachPlan so the launcher (which compiles on
// non-linux too) handles an opaque plan. ip() exposes the reserved address for
// readiness probing; the hook env methods render the paired OCI hook inputs.
type attachPlan struct{ cap *spacenet.AttachPlan }

// ip is the reserved container IP, recorded for native readiness probing.
func (p *attachPlan) ip() string { return p.cap.IP }

// moveHookEnv is consumed by createRuntime to move the peer into the netns.
func (p *attachPlan) moveHookEnv() []string { return spacenet.HookMoveEnv(p.cap) }

// configureHookEnv is consumed by createContainer from inside that netns.
func (p *attachPlan) configureHookEnv() []string { return spacenet.HookConfigureEnv(p.cap) }

// defaultSpaceSubnet is the bridge CIDR every space network is built on.
// Space-private (the worker's netns is destroyed on Close), so the same
// range is reused per space without host-side collision. .1 is the gateway
// (resolver + default route), .2+ are handed to attached service containers.
const defaultSpaceSubnet = "10.88.0.0/24"

// spaceNet is the runtime's handle on one space's network. It wraps
// spacenet.SpaceNet so the deploy launcher (which must also compile on
// non-linux, where spacenet does not exist) talks to a small in-package
// interface instead of spacenet directly. One spaceNet is shared by every
// service of a space for the space's lifetime.
type spaceNet struct {
	cnet       *spacenet.SpaceNet
	stopEgr    func() // egress teardown; nil when egress was unavailable
	resolv     string // bind-mount target for /etc/resolv.conf
	hosts      string // bind-mount target for /etc/hosts
	usernsPath string
}

// newSpaceNet creates the space bridge network (spacenet worker + bridge +
// DNS on the gateway) and best-effort enables Internet egress for the whole
// subnet. Egress failure is NOT fatal: the internal bridge (sibling
// reachability + local DNS) keeps working; only outbound Internet is lost.
//
// REQUIREMENT: the host binary MUST call spacenet.MaybeRunWorker() early in
// main() — the space worker is a re-exec of this same binary into a fresh
// user+net namespace. cmd/sklp wires this via maybeRunSpaceNetWorker().
func newSpaceNet(ctx context.Context, name, slirpBin string, allowHost bool, allowEgress []string, attachSock string) (*spaceNet, error) {
	cnet, err := spacenet.NewSpaceNetAt(name, defaultSpaceSubnet, attachSock)
	if err != nil {
		return nil, err
	}
	resolv, err := cnet.ResolvConfPath()
	if err != nil {
		cnet.Close()
		return nil, err
	}
	hosts, err := cnet.HostsPath()
	if err != nil {
		cnet.Close()
		return nil, err
	}
	cn := &spaceNet{
		cnet:       cnet,
		resolv:     resolv,
		hosts:      hosts,
		usernsPath: cnet.Endpoints().UsernsPath,
	}
	// No slirp binary means the caller asked for a space without Internet
	// (tests, a deliberately offline stack): say so once and go on. Any other
	// egress failure — slirp4netns dying on /dev/net/tun, nft refusing the NAT
	// — is the space's failure: every apt, go mod download and image pull
	// would otherwise fail minutes later with an error that names DNS.
	if slirpBin == "" {
		fmt.Fprintf(os.Stderr, "[sklp] space %q: no slirp4netns, services have no Internet\n", name)
		return cn, nil
	}
	// Every Space with egress reaches this host's own wg0 (ADR-008) and the
	// Network Controller unconditionally: DNS resolvability and cross-Space
	// reachability are now decided entirely by whether a service declares
	// port:, not by a per-Space opt-in — see allowcontroller_linux.go.
	stop, eerr := cnet.EnableEgress(ctx, slirpBin, allowHost, true, allowEgress, os.Getenv(spacenet.EnvControllerIP))
	if eerr != nil {
		cnet.Close()
		return nil, fmt.Errorf("space egress: %w (run `sklp host check`)", eerr)
	}
	cn.stopEgr = stop
	return cn, nil
}

// join returns the namespace-join descriptor for a space service: it joins
// the space USER namespace (so spacenet may push a veth into the container's
// netns) while keeping a PRIVATE network namespace the bridge attaches after
// launch.
func (c *spaceNet) join() *joinNamespaces {
	return &joinNamespaces{UsernsPath: c.usernsPath, PrivateNetns: true}
}

// identity is who a service of this space is on the host: the mapping the
// space worker was given, which every service joins.
func (c *spaceNet) identity() ociproc.Identity { return c.cnet.Identity() }

// serviceIdentity is the identity a service launched through cnet gets. With
// no space network the launcher runs standalone containers mapped from the
// caller's own grant, or from the caller alone when it has none.
func serviceIdentity(cnet *spaceNet) ociproc.Identity {
	if cnet != nil {
		return cnet.identity()
	}
	if id, err := ociproc.ResolveIdentity(os.Getuid(), os.Getgid()); err == nil {
		return id
	}
	return ociproc.SingleIdentity(os.Getuid(), os.Getgid())
}

// spawnOverlay forks a per-service overlay holder in the space userns and
// returns its host-visible pid (the namespace runc is nsenter'd into). See
// spacenet.SpawnOverlay.
func (c *spaceNet) spawnOverlay(lower, upper, work, merged string) (int, error) {
	return c.cnet.SpawnOverlay(lower, upper, work, merged)
}

// spawnOverlayChild is spawnOverlay with the holder in a child user namespace:
// the service's root is a subuid, not the operator. See spacenet.SpawnOverlayChild.
func (c *spaceNet) spawnOverlayChild(lower, upper, work, merged string, slot int) (int, ociproc.Identity, error) {
	return c.cnet.SpawnOverlayChild(lower, upper, work, merged, slot)
}

// enterAsSpaceRoot is the nsenter prefix that runs a host command as the root
// of the space user namespace: the operator, with capabilities over every
// child namespace and every id the space maps.
func (c *spaceNet) enterAsSpaceRoot() []string {
	return ociproc.EnterArgs(c.cnet.WorkerPID(), false)
}

func (c *spaceNet) runAsSpaceRoot(argv ...string) error {
	full := append(append(c.enterAsSpaceRoot(), "--"), argv...)
	var out []byte
	var err error
	// A worker that has just been mapped is not enterable for a moment (its
	// /proc entries still belong to root until its re-exec): retry briefly.
	for attempt := 0; attempt < 20; attempt++ {
		out, err = exec.Command("nsenter", full...).CombinedOutput()
		// ECHILD: the SIGCHLD subreaper harvested nsenter's status; it ran
		// (see spacenet.runMapHelper). Retrying a mv or cp that already
		// happened is what would fail.
		if err == nil || errors.Is(err, unix.ECHILD) {
			return nil
		}
		if !strings.Contains(string(out), "cannot open /proc/") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("%s in space userns: %w: %s", argv[0], err, out)
}

// handToChild gives paths to a child identity's root. The operator cannot
// chown into the delegated range (that takes CAP_CHOWN); the space root can,
// and inside the space the child's root is the plain id childRootID.
func (c *spaceNet) handToChild(childRootID int, paths ...string) error {
	owner := fmt.Sprintf("%d:%d", childRootID, childRootID)
	return c.runAsSpaceRoot(append([]string{"chown", "-R", owner}, paths...)...)
}

// reclaimFromChild takes paths back from a child identity: inside the space,
// id 0 is the operator.
func (c *spaceNet) reclaimFromChild(paths ...string) error {
	return c.runAsSpaceRoot(append([]string{"chown", "-R", "0:0"}, paths...)...)
}

// killAsSpaceRoot signals a process that runs as a subuid the operator may
// not signal directly.
func (c *spaceNet) killAsSpaceRoot(pid int) error {
	return c.runAsSpaceRoot("kill", "-9", strconv.Itoa(pid))
}

// resolvConf is the host path to bind-mount at /etc/resolv.conf so the service
// resolves names through the space gateway resolver.
func (c *spaceNet) resolvConf() string { return c.resolv }

// hostsConf is the host path to bind-mount at /etc/hosts so the service can
// resolve loopback (localhost / 0.0.0.0) — the space netns ships none.
func (c *spaceNet) hostsConf() string { return c.hosts }

// attach wires a launched service container (its init pid) onto the space
// bridge under serviceName, registering name->IP in the space DNS so
// siblings resolve it. Replicas share one name (spacenet round-robins). The
// returned IP is informational; routing is by name.
func (c *spaceNet) attach(serviceName string, pid int) (string, error) {
	return c.cnet.Attach(serviceName, pid, "eth0")
}

// hookAttachReady reports whether the worker's attach socket is wired, i.e.
// whether the launcher can attach the network via the createRuntime hook
// (before the container's command execs) instead of the post-start attach.
func (c *spaceNet) hookAttachReady() bool { return c.cnet.AttachSockReady() }

// setL7Routes pushes the native L7 ingress routing table to the worker (which
// hot-swaps its in-netns reverse proxy). routesJSON is a JSON-encoded
// []ingress.Route; "" clears the table.
func (c *spaceNet) setL7Routes(routesJSON string) error { return c.cnet.SetL7Routes(routesJSON) }

// setACLEdges pushes the declared depends_on graph to the worker, which
// reconciles the bridge's east-west forward chain toward it (default-deny,
// only the declared edges open). edges carries service NAMES; the worker
// resolves them to live IPs via its own name table at push time.
func (c *spaceNet) setACLEdges(edges []ACLEdge) error {
	converted := make([]spacenet.ACLEdge, len(edges))
	for i, e := range edges {
		converted[i] = spacenet.ACLEdge{From: e.From, To: e.To}
	}
	return c.cnet.SetACLEdges(converted)
}

// planAttach reserves the IP + veth names the createRuntime hook will use for a
// service, BEFORE its container starts. The returned attachPlan is embedded as
// hook args in the service spec; bindAttach records it (once the pid is known)
// so detach/close undo it, and releaseAttach reclaims the IP if the container
// never starts.
func (c *spaceNet) planAttach(serviceName string) (*attachPlan, error) {
	p, err := c.cnet.PlanAttach(serviceName, "eth0")
	if err != nil {
		return nil, err
	}
	return &attachPlan{cap: p}, nil
}

// bindAttach records a hook-completed attach against its container pid so a
// later detach removes the veth and reclaims the IP.
func (c *spaceNet) bindAttach(p *attachPlan, pid int) { c.cnet.BindAttach(p.cap, pid) }

// releaseAttach returns a planned attach's IP to the pool when the container
// never started (the hook never ran), avoiding an IPAM leak.
func (c *spaceNet) releaseAttach(p *attachPlan) { c.cnet.ReleaseAttach(p.cap) }

// cancelAttach unwinds a plan whose hook may have run before runc later failed.
func (c *spaceNet) cancelAttach(p *attachPlan) { c.cnet.CancelAttach(p.cap) }

// detach removes a single container from the bridge (idempotent). Called when
// one instance stops while the space keeps running.
func (c *spaceNet) detach(pid int) { _ = c.cnet.Detach(pid) }

// sendConn hands an accepted host connection to the space worker, which dials
// the next replica of serviceName on containerPort FROM the space netns and
// splices the two ends. This is the host->space ingress path; the round-robin
// across replicas is the space DNS name table's rotation (single source of
// truth for the replica IP set).
func (c *spaceNet) sendPacketConn(serviceName string, containerPort int, pc *net.UDPConn) error {
	return c.cnet.SendPacketConn(serviceName, containerPort, pc)
}

func (c *spaceNet) sendConn(serviceName string, containerPort int, proxyProtocol bool, conn net.Conn) error {
	return c.cnet.SendConn(serviceName, containerPort, proxyProtocol, conn)
}

// sendConnReplay splices client to serviceName:containerPort in the space netns
// AFTER the host mux has peeked the HTTP Host header (consuming prefix bytes from
// client). sendConn fd-passes the client socket to the worker, which reads from
// it directly — so the already-consumed prefix would be lost. To reuse that
// netns splice intact, we hairpin through a transient loopback TCP pair: the
// worker splices the pair's server end to the backend; the host splices client
// to the pair's mux end, writing prefix first. Bytes flow
// client <-> muxSide ~loopback~ srvSide <-> backend (in netns). One extra hop on
// 127.0.0.1, but the single netns ingress primitive (sendConn) is unchanged.
func (c *spaceNet) sendConnReplay(serviceName string, containerPort int, client net.Conn, prefix []byte) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("host mux hairpin listen: %w", err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, _ := ln.Accept()
		accepted <- conn
	}()
	muxSide, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		return fmt.Errorf("host mux hairpin dial: %w", err)
	}
	srvSide := <-accepted
	if srvSide == nil {
		muxSide.Close()
		return fmt.Errorf("host mux hairpin accept failed")
	}
	// Hand the server end to the worker: it dials the backend in the netns and
	// splices srvSide<->backend. sendConn takes ownership of srvSide.
	if err := c.cnet.SendConn(serviceName, containerPort, false, srvSide); err != nil {
		// SendConn takes ownership of srvSide only on success; close both ends on
		// failure so the loopback hairpin fds aren't leaked per failed request.
		srvSide.Close()
		muxSide.Close()
		return err
	}
	go hairpinSplice(client, muxSide, prefix)
	return nil
}

// hairpinSplice copies client<->muxSide both ways until either closes, writing
// the peeked prefix into muxSide first so the backend sees the full request. It
// owns both conns (closes them on exit) — the caller has already handed srvSide
// to the worker.
func hairpinSplice(client, muxSide net.Conn, prefix []byte) {
	if len(prefix) > 0 {
		if _, err := muxSide.Write(prefix); err != nil {
			client.Close()
			muxSide.Close()
			return
		}
	}
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		client.Close()
		muxSide.Close()
	}
	go cp(muxSide, client)
	go cp(client, muxSide)
	wg.Wait()
}

// probeTCP dials addr ("ip:port") from the space netns. The bridge netns
// reaches every attached container IP, so a readiness probe connects to a
// service's bridge IP natively — no dependency on a `nc` binary in the image.
func (c *spaceNet) probeTCP(addr string) bool { return c.cnet.ProbeTCP(addr) }

// probeHTTP GETs url from the space netns (2xx = ready), same reachability as
// probeTCP and no dependency on a `wget` binary in the image.
func (c *spaceNet) probeHTTP(url string) bool { return c.cnet.ProbeHTTP(url) }

// close tears the whole space network down: stop egress, then destroy the
// worker netns (which takes the bridge and every veth with it). No leaks.
func (c *spaceNet) close() {
	if c.stopEgr != nil {
		c.stopEgr()
	}
	c.cnet.Close()
}
