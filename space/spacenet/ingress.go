//go:build linux

package spacenet

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/vishvananda/netns"
)

// ingressFD is the inherited fd of the host<->worker control socket in the
// WORKER process. The worker is re-exec'd with the worker end of a SEQPACKET
// socketpair as its first ExtraFile, which lands at fd 3 (after stdin/out/err).
// This is a SECOND channel, separate from the line-delimited JSON pipe, so a
// long-lived proxy splice in the worker never blocks the request/reply mutex.
const ingressFD = 3

// ingressEnv is set on the re-exec'd worker iff a control socket was wired, so
// the worker only serves ingress when the host opted in (it is harmless to skip
// otherwise — the JSON protocol is unaffected).
const ingressEnv = "SKLP_SPACE_INGRESS"

// ingressRequest is the host's per-connection ask, sent as the payload of a
// SCM_RIGHTS message whose single fd is the accepted CLIENT socket. The worker
// dials Service's next replica on Port FROM the space netns and splices the
// two ends. Service is a registered space DNS name; Port is the container port.
type ingressRequest struct {
	Service string `json:"service"`
	Port    int    `json:"port"`
	// ProxyProtocol asks the worker to prefix the backend stream with a PROXY
	// protocol v2 header carrying the client's real endpoints.
	ProxyProtocol bool `json:"proxy_protocol,omitempty"`
	// UDP marks the fd as a bound host UDP socket the worker relays datagrams
	// from for as long as it lives, rather than one accepted connection.
	UDP bool `json:"udp,omitempty"`
}

// DialBackend names how a worker picks and reaches a backend for one ingress
// connection: it is the nameTable lookup (rotated replica IPs — the SAME
// round-robin the space DNS uses) plus the netns-pinned dial. Kept as a tiny
// indirection so the splice logic is unit-testable with an in-memory dialer.
type backendDialer func(service string, port int) (net.Conn, error)

// newIngressPair builds the host<->worker control socketpair: a connected
// SEQPACKET pair (message boundaries preserved, so one SCM_RIGHTS send == one
// worker recv). It returns the host end as a *net.UnixConn and the worker end as
// an *os.File to pass via ExtraFiles. Best-effort: any failure returns (nil, nil)
// so the space still works east-west — only host ingress is unavailable.
func newIngressPair() (*net.UnixConn, *os.File) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET, 0)
	if err != nil {
		return nil, nil
	}
	hostFile := os.NewFile(uintptr(fds[0]), "space-ingress-host")
	workerFile := os.NewFile(uintptr(fds[1]), "space-ingress-worker")
	if hostFile == nil || workerFile == nil {
		_ = syscall.Close(fds[0])
		_ = syscall.Close(fds[1])
		return nil, nil
	}
	conn, err := net.FileConn(hostFile)
	_ = hostFile.Close() // FileConn dups; drop our copy
	if err != nil {
		_ = workerFile.Close()
		return nil, nil
	}
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		conn.Close()
		_ = workerFile.Close()
		return nil, nil
	}
	return uc, workerFile
}

// SendPacketConn hands a bound host UDP socket to the worker, which relays its
// datagrams to service on port from the space netns until the space ends. The
// caller's copy is closed either way.
func (c *SpaceNet) SendPacketConn(service string, port int, pc *net.UDPConn) error {
	defer pc.Close()
	if c.ctrl == nil {
		return fmt.Errorf("spacenet: ingress not available for %q", c.name)
	}
	raw, err := pc.SyscallConn()
	if err != nil {
		return fmt.Errorf("spacenet: ingress raw udp conn: %w", err)
	}
	payload, err := json.Marshal(ingressRequest{Service: service, Port: port, UDP: true})
	if err != nil {
		return err
	}
	var sendErr error
	ctrlErr := raw.Control(func(fd uintptr) {
		c.ctrlMu.Lock()
		_, _, sendErr = c.ctrl.WriteMsgUnix(payload, syscall.UnixRights(int(fd)), nil)
		c.ctrlMu.Unlock()
	})
	if ctrlErr != nil {
		return fmt.Errorf("spacenet: ingress control: %w", ctrlErr)
	}
	if sendErr != nil {
		return fmt.Errorf("spacenet: ingress send udp: %w", sendErr)
	}
	return nil
}

// SendConn hands one accepted client connection to the worker for splicing to a
// replica of service on port. It extracts the connection's raw fd and writes it
// (SCM_RIGHTS) plus the ingressRequest as one atomic SEQPACKET message. The
// worker owns the fd after a successful send; the caller's conn is closed here
// either way (the worker holds its own dup). Returns an error when ingress was
// not wired or the send fails — the caller then closes/refuses the client.
func (c *SpaceNet) SendConn(service string, port int, proxyProtocol bool, conn net.Conn) error {
	if c.ctrl == nil {
		return fmt.Errorf("spacenet: ingress not available for %q", c.name)
	}
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return fmt.Errorf("spacenet: ingress expects a *net.TCPConn, got %T", conn)
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return fmt.Errorf("spacenet: ingress raw conn: %w", err)
	}
	payload, err := json.Marshal(ingressRequest{Service: service, Port: port, ProxyProtocol: proxyProtocol})
	if err != nil {
		return err
	}

	var sendErr error
	ctrlErr := raw.Control(func(fd uintptr) {
		rights := syscall.UnixRights(int(fd))
		c.ctrlMu.Lock()
		_, _, sendErr = c.ctrl.WriteMsgUnix(payload, rights, nil)
		c.ctrlMu.Unlock()
	})
	// The worker has its own dup of the fd now; closing our conn does not affect
	// it. Close regardless so the host never leaks one socket per connection.
	_ = conn.Close()
	if ctrlErr != nil {
		return fmt.Errorf("spacenet: ingress control: %w", ctrlErr)
	}
	if sendErr != nil {
		return fmt.Errorf("spacenet: ingress send: %w", sendErr)
	}
	return nil
}

// startIngressServer runs in the worker: it adopts the inherited control fd
// (ingressFD) and serves ingress on it in a background goroutine, so the worker
// continues processing JSON commands on stdin. A bad/absent fd only disables
// ingress — east-west networking is untouched.
func startIngressServer(capNS netns.NsHandle, names *nameTable) {
	f := os.NewFile(uintptr(ingressFD), "space-ingress-worker")
	if f == nil {
		return
	}
	conn, err := net.FileConn(f)
	_ = f.Close() // FileConn dups
	if err != nil {
		return
	}
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		conn.Close()
		return
	}
	go serveIngress(capNS, names, uc)
}

// serveIngress runs in the worker: it reads SCM_RIGHTS messages off the control
// socket (each carrying one accepted client fd + an ingressRequest), and for
// each spawns a goroutine that dials the service's next replica IN THE SPACE
// NETNS and splices the client to it. It returns when the socket closes (host
// gone / teardown). names is the shared service table; capNS the space netns.
func serveIngress(capNS netns.NsHandle, names *nameTable, ctrl *net.UnixConn) {
	dial := func(service string, port int) (net.Conn, error) {
		return dialReplicaInNetns(capNS, names, service, port)
	}
	for {
		clientFD, req, err := recvConnRequest(ctrl)
		if err != nil {
			return // socket closed: host gone, stop serving
		}
		if req.UDP {
			go handleIngressUDP(clientFD, req, capNS, names)
			continue
		}
		go handleIngressConn(clientFD, req, dial)
	}
}

func handleIngressUDP(fd int, req ingressRequest, capNS netns.NsHandle, names *nameTable) {
	conn, err := connFromFD(fd)
	if err != nil {
		_ = syscall.Close(fd)
		return
	}
	pub, ok := conn.(*net.UDPConn)
	if !ok {
		_ = conn.Close()
		return
	}
	defer pub.Close()
	dial, stop := netnsUDPDialer(capNS, names, req.Service, req.Port)
	defer stop()
	relayUDP(pub, dial, udpFlowIdle, udpMaxFlows)
}

// handleIngressConn turns a received client fd into a net.Conn, dials the
// backend, and splices them. A backend dial failure closes the client (the
// 502-equivalent for raw TCP: the client sees a closed connection).
func handleIngressConn(clientFD int, req ingressRequest, dial backendDialer) {
	client, err := connFromFD(clientFD)
	if err != nil {
		_ = syscall.Close(clientFD)
		return
	}
	defer client.Close()

	backend, err := dial(req.Service, req.Port)
	if err != nil {
		return // no Ready replica reachable: drop the client (closed conn)
	}
	defer backend.Close()
	if req.ProxyProtocol {
		if _, err := backend.Write(proxyProtocolV2Header(client.RemoteAddr(), client.LocalAddr())); err != nil {
			return
		}
	}
	splice(client, backend)
}

// dialReplicaInNetns picks the next replica IP for service (rotated by the same
// nameTable round-robin the DNS uses) and dials port on it FROM the space
// netns — the exact netns hop ProbeTCP/Dial use: lock the OS thread, enter capNS,
// net.Dial, and the locked thread is discarded (never unlocked) so the dialed
// socket cannot migrate to a host-netns thread. On a replica that refuses, it
// falls through to the next rotated IP before giving up.
func dialReplicaInNetns(capNS netns.NsHandle, names *nameTable, service string, port int) (net.Conn, error) {
	ips, ok := names.lookup(service)
	if !ok || len(ips) == 0 {
		return nil, fmt.Errorf("ingress: service %q has no ready replica", service)
	}

	// Pin a dedicated OS thread to the space netns for the lifetime of this
	// connection's dial. We do NOT UnlockOSThread: the connected socket stays
	// valid regardless of scheduling, and the goroutine ends after the splice,
	// so the Go runtime discards the (now host-netns-dirty) thread.
	runtime.LockOSThread()
	if err := netns.Set(capNS); err != nil {
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("ingress: enter space netns: %w", err)
	}

	var lastErr error
	for _, ip := range ips {
		addr := net.JoinHostPort(ip.String(), fmt.Sprintf("%d", port))
		conn, err := net.DialTimeout("tcp", addr, probeTimeout)
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("ingress: dial %q replicas: %w", service, lastErr)
}

// splice copies bytes in both directions until both sides have finished. A
// source EOF half-closes the destination's write side (so its peer sees a
// clean FIN, exactly like a direct connection) instead of closing the whole
// connection outright — a client that writes a request then half-closes
// (e.g. `nc < file`, or any client done sending before it starts reading)
// would otherwise have its still-arriving response severed the instant its
// own request finished sending. Both conns are closed fully only once
// neither goroutine can make further progress.
func splice(a, b net.Conn) {
	// An IDLE deadline, rearmed on every transfer, rather than an absolute one:
	// a passthrough route is raw TCP, so nothing else on this path can ever
	// close a connection that goes silent — the mux left the path once the fd
	// was passed, and the L7 is not in it at all. An absolute deadline would
	// instead cut legitimate long-lived traffic (a big download, a WebSocket).
	// Both directions share the clock, so traffic either way keeps the
	// connection alive; only a connection with NO activity at all is dropped.
	spliceWithIdle(a, b, spliceIdleTimeout)
}

// spliceWithIdle is splice with an injectable idle timeout, so the deadline
// behaviour is testable without waiting out the production value.
func spliceWithIdle(a, b net.Conn, timeout time.Duration) {
	idle := newIdleDeadline(a, b, timeout)
	idle.arm()

	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, idle.reader(src))
		closeWrite(dst)
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
	_ = a.Close()
	_ = b.Close()
}

// spliceIdleTimeout is how long a passthrough connection may sit with no bytes
// moving in EITHER direction before both ends are closed. Generous on purpose:
// idle-but-alive is a normal state for a keep-alive connection between
// requests, and this is the only thing bounding it.
const spliceIdleTimeout = 10 * time.Minute

// idleDeadline rearms a read deadline on both conns whenever either direction
// transfers bytes. Safe for concurrent use by the two copy goroutines.
type idleDeadline struct {
	conns   [2]net.Conn
	timeout time.Duration

	mu   sync.Mutex
	last time.Time
}

func newIdleDeadline(a, b net.Conn, timeout time.Duration) *idleDeadline {
	return &idleDeadline{conns: [2]net.Conn{a, b}, timeout: timeout}
}

// arm sets the initial deadline on both ends.
func (d *idleDeadline) arm() { d.bump(true) }

// bump pushes both read deadlines out by the timeout. Rate-limited to once per
// timeout/4 so a busy connection does not pay a syscall pair per Read.
func (d *idleDeadline) bump(force bool) {
	now := time.Now()
	d.mu.Lock()
	if !force && now.Sub(d.last) < d.timeout/4 {
		d.mu.Unlock()
		return
	}
	d.last = now
	d.mu.Unlock()
	for _, c := range d.conns {
		_ = c.SetReadDeadline(now.Add(d.timeout))
	}
}

// reader wraps src so every successful Read rearms the shared deadline.
func (d *idleDeadline) reader(src net.Conn) io.Reader {
	return &idleReader{d: d, src: src}
}

type idleReader struct {
	d   *idleDeadline
	src net.Conn
}

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	if n > 0 {
		r.d.bump(false)
	}
	return n, err
}

// closeWrite half-closes dst's write side if it supports it (every conn
// splice is called with here is a *net.TCPConn), falling back to a full
// Close for a conn type that does not.
func closeWrite(dst net.Conn) {
	type writeCloser interface{ CloseWrite() error }
	if wc, ok := dst.(writeCloser); ok {
		_ = wc.CloseWrite()
		return
	}
	_ = dst.Close()
}

// recvConnRequest reads one SCM_RIGHTS message off the control socket: the
// out-of-band data carries exactly one fd (the accepted client socket) and the
// in-band data is the JSON ingressRequest. An error (incl. a closed socket)
// stops the worker's ingress loop.
func recvConnRequest(ctrl *net.UnixConn) (int, ingressRequest, error) {
	buf := make([]byte, 1024)
	oob := make([]byte, syscall.CmsgSpace(4)) // room for one fd
	n, oobn, _, _, err := ctrl.ReadMsgUnix(buf, oob)
	if err != nil {
		return -1, ingressRequest{}, err
	}
	fd, err := parseSingleFD(oob[:oobn])
	if err != nil {
		return -1, ingressRequest{}, err
	}
	var req ingressRequest
	if err := json.Unmarshal(buf[:n], &req); err != nil {
		_ = syscall.Close(fd)
		return -1, ingressRequest{}, fmt.Errorf("ingress: decode request: %w", err)
	}
	return fd, req, nil
}

// parseSingleFD extracts exactly one fd from a SCM_RIGHTS control message.
func parseSingleFD(oob []byte) (int, error) {
	scms, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return -1, fmt.Errorf("ingress: parse control message: %w", err)
	}
	if len(scms) != 1 {
		return -1, fmt.Errorf("ingress: expected 1 control message, got %d", len(scms))
	}
	fds, err := syscall.ParseUnixRights(&scms[0])
	if err != nil {
		return -1, fmt.Errorf("ingress: parse fd rights: %w", err)
	}
	if len(fds) != 1 {
		// Close any stray fds so we never leak a passed descriptor.
		for _, fd := range fds {
			_ = syscall.Close(fd)
		}
		return -1, fmt.Errorf("ingress: expected 1 fd, got %d", len(fds))
	}
	return fds[0], nil
}

// connFromFD wraps a received socket fd in a *net.TCPConn. os.NewFile dups the
// fd into the runtime poller via FileConn, so we close our original copy after.
func connFromFD(fd int) (net.Conn, error) {
	f := os.NewFile(uintptr(fd), "ingress-client")
	if f == nil {
		return nil, fmt.Errorf("ingress: invalid client fd %d", fd)
	}
	conn, err := net.FileConn(f)
	_ = f.Close() // FileConn dups; drop our copy either way
	if err != nil {
		return nil, fmt.Errorf("ingress: client conn from fd: %w", err)
	}
	return conn, nil
}
