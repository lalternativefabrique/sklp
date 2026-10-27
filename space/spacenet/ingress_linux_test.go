//go:build linux

package spacenet

import (
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestNameTableRoundRobin asserts the SAME rotation the proxy LB reuses: over a
// service with 3 IPs, three successive lookups put each IP first in turn, so a
// proxy that dials lookup()[0] spreads connections round-robin across replicas.
func TestNameTableRoundRobin(t *testing.T) {
	nt := newNameTable()
	ips := []string{"10.88.0.2", "10.88.0.3", "10.88.0.4"}
	for _, ip := range ips {
		nt.register("web", net.ParseIP(ip))
	}

	var firsts []string
	for i := 0; i < 3; i++ {
		got, ok := nt.lookup("web")
		if !ok || len(got) != 3 {
			t.Fatalf("lookup #%d: ok=%v len=%d", i, ok, len(got))
		}
		firsts = append(firsts, got[0].String())
	}
	seen := map[string]bool{}
	for _, f := range firsts {
		seen[f] = true
	}
	if len(seen) != 3 {
		t.Fatalf("3 successive lookups touched %d distinct replicas (%v), want 3", len(seen), firsts)
	}
}

// TestSpliceBidirectional proves the proxy's byte pump copies BOTH directions
// and tears down cleanly once both sides have closed (no goroutine leak:
// splice returns once both io.Copy halts). Closing only ONE side now half-
// closes its peer's write side instead of ending the whole splice — a real
// client that finishes sending before it finishes reading (e.g. `nc < file`)
// must still see its response, which a same-instant full close would sever.
func TestSpliceBidirectional(t *testing.T) {
	// a <-> aPeer and b <-> bPeer; splice(a,b) bridges aPeer's writes to bPeer.
	a, aPeer := mustPipeConn(t)
	b, bPeer := mustPipeConn(t)
	defer aPeer.Close()
	defer bPeer.Close()

	done := make(chan struct{})
	go func() { splice(a, b); close(done) }()

	// client -> backend
	if _, err := aPeer.Write([]byte("ping")); err != nil {
		t.Fatalf("write client: %v", err)
	}
	if got := mustRead(t, bPeer, 4); got != "ping" {
		t.Fatalf("backend got %q, want ping", got)
	}
	// backend -> client
	if _, err := bPeer.Write([]byte("pong")); err != nil {
		t.Fatalf("write backend: %v", err)
	}
	if got := mustRead(t, aPeer, 4); got != "pong" {
		t.Fatalf("client got %q, want pong", got)
	}

	// Closing aPeer only EOFs one copy (client done sending); splice must
	// still be waiting for the other direction, half-closed, not torn down.
	aPeer.Close()
	select {
	case <-done:
		t.Fatal("splice returned after only one side closed — the other direction was cut short")
	case <-time.After(200 * time.Millisecond):
	}

	// Closing bPeer too finishes the other direction; splice now returns.
	bPeer.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("splice did not return after both sides closed (goroutine leak)")
	}
}

// TestHandleIngressConnRoundRobin drives handleIngressConn with an injected
// dialer round-robining over THREE in-memory backends and asserts each backend
// is hit once across three connections, and that bytes flow both ways. This is
// the proxy core without a real netns (the netns hop is dialReplicaInNetns,
// exercised by the integration path).
func TestHandleIngressConnRoundRobin(t *testing.T) {
	const n = 3
	listeners := make([]net.Listener, n)
	var hits [n]int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen backend %d: %v", i, err)
		}
		defer ln.Close()
		listeners[i] = ln
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Serve exactly one connection: echo "hello" back, count the hit.
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			atomic.AddInt32(&hits[i], 1)
			buf := make([]byte, 5)
			if _, err := io.ReadFull(conn, buf); err != nil {
				return
			}
			_, _ = conn.Write(buf) // echo
		}()
	}

	var cursor int32
	dial := func(service string, port int) (net.Conn, error) {
		if service != "web" {
			return nil, fmt.Errorf("unexpected service %q", service)
		}
		idx := int(atomic.AddInt32(&cursor, 1)-1) % n
		return net.Dial("tcp", listeners[idx].Addr().String())
	}

	for c := 0; c < n; c++ {
		clientFD, clientEnd := mustSocketpairConn(t)
		// handleIngressConn owns clientFD; run it in a goroutine.
		go handleIngressConn(clientFD, ingressRequest{Service: "web", Port: 1234}, dial)

		if _, err := clientEnd.Write([]byte("hello")); err != nil {
			t.Fatalf("conn %d write: %v", c, err)
		}
		got := mustRead(t, clientEnd, 5)
		if got != "hello" {
			t.Fatalf("conn %d echo = %q, want hello", c, got)
		}
		clientEnd.Close()
	}

	wg.Wait()
	for i := 0; i < n; i++ {
		if atomic.LoadInt32(&hits[i]) != 1 {
			t.Fatalf("backend %d hit %d times, want 1 (round-robin broken)", i, hits[i])
		}
	}
}

// TestHandleIngressConnBackendDown closes the client when no backend can be
// dialed (the 502-equivalent for raw TCP).
func TestHandleIngressConnBackendDown(t *testing.T) {
	dial := func(string, int) (net.Conn, error) {
		return nil, fmt.Errorf("no ready replica")
	}
	clientFD, clientEnd := mustSocketpairConn(t)
	done := make(chan struct{})
	go func() { handleIngressConn(clientFD, ingressRequest{Service: "web", Port: 80}, dial); close(done) }()

	// The client read should return EOF promptly (handleIngressConn closed it).
	clientEnd.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := clientEnd.Read(buf); err == nil {
		t.Fatal("expected client connection to be closed when backend is down")
	}
	clientEnd.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleIngressConn did not return on backend-down")
	}
}

// TestSendConnRoundTrip proves the host->worker fd-passing protocol carries an
// accepted client socket AND its ingressRequest intact: SendConn writes the fd
// (SCM_RIGHTS) + JSON, recvConnRequest reconstructs both, and bytes flow over
// the reconstructed worker-side conn. This is the half of the path that does NOT
// need the privileged netns hop (which only the real space worker can do; the
// integration test covers it). It mirrors how an accepted listener conn reaches
// the worker.
func TestSendConnRoundTrip(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	hostConn := mustUnixConn(t, fds[0])
	workerConn := mustUnixConn(t, fds[1])
	defer hostConn.Close()
	defer workerConn.Close()

	cn := &SpaceNet{name: "test", ctrl: hostConn}

	clientA, clientB := mustPipeConn(t)
	defer clientB.Close()
	if err := cn.SendConn("web", 8080, false, clientA.(*net.TCPConn)); err != nil {
		t.Fatalf("SendConn: %v", err)
	}

	fd, req, err := recvConnRequest(workerConn)
	if err != nil {
		t.Fatalf("recvConnRequest: %v", err)
	}
	if req.Service != "web" || req.Port != 8080 {
		t.Fatalf("request = %+v, want {web 8080}", req)
	}
	wc, err := connFromFD(fd)
	if err != nil {
		t.Fatalf("connFromFD: %v", err)
	}
	defer wc.Close()

	// clientB writes; the worker-side conn (other end of the passed fd) reads it,
	// proving the SAME socket crossed the process boundary.
	go func() { _, _ = clientB.Write([]byte("bytes")) }()
	if got := mustRead(t, wc, 5); got != "bytes" {
		t.Fatalf("worker-side read = %q, want bytes", got)
	}
}

func mustUnixConn(t *testing.T, fd int) *net.UnixConn {
	t.Helper()
	f := os.NewFile(uintptr(fd), "ctrl")
	c, err := net.FileConn(f)
	_ = f.Close()
	if err != nil {
		t.Fatalf("FileConn ctrl: %v", err)
	}
	uc, ok := c.(*net.UnixConn)
	if !ok {
		t.Fatalf("ctrl is %T, want *net.UnixConn", c)
	}
	return uc
}

// TestIngressDialsInSpaceNetns is the integration go/no-go: a REAL space
// worker dials a backend FROM the space netns on behalf of a host connection.
// The space DNS resolver listens on gateway:53 (TCP) INSIDE the space netns
// — a backend reachable ONLY from within it — so registering the gateway as a
// service and SendConn'ing to :53 proves the worker performed the privileged
// netns hop the host itself cannot. We assert the spliced connection STAYS open
// (the DNS-over-TCP server awaits a length-prefixed query) rather than being
// reset, which is exactly the "backend down" failure path. Skipped where rootless
// user+net namespaces are unavailable.
func TestIngressDialsInSpaceNetns(t *testing.T) {
	skipUnlessRootlessNet(t)

	cn, err := NewSpaceNet("ingress-it", "10.88.0.0/24")
	if err != nil {
		t.Fatalf("NewSpaceNet: %v", err)
	}
	defer cn.Close()
	if cn.ctrl == nil {
		t.Fatal("ingress control socket was not wired")
	}

	// Register the gateway (where the in-netns DNS TCP server listens) under a
	// service name, then drive a host connection to it on port 53.
	gw := cn.Endpoints().Gateway
	if _, err := cn.send(command{Op: "register", Name: "resolver", IP: gw}); err != nil {
		t.Fatalf("register: %v", err)
	}

	clientA, clientB := mustPipeConn(t)
	defer clientB.Close()
	if err := cn.SendConn("resolver", 53, false, clientA.(*net.TCPConn)); err != nil {
		t.Fatalf("SendConn: %v", err)
	}

	// If the netns dial FAILED, handleIngressConn closes the client and clientB
	// reads EOF/reset immediately. If it SUCCEEDED, the DNS TCP server holds the
	// connection open waiting for a query, so the read blocks -> deadline.
	clientB.SetReadDeadline(time.Now().Add(750 * time.Millisecond))
	buf := make([]byte, 1)
	_, rerr := clientB.Read(buf)
	if ne, ok := rerr.(net.Error); ok && ne.Timeout() {
		return // connection alive: the worker dialed the backend in the space netns
	}
	t.Fatalf("expected the spliced connection to stay open (dialed in netns), got read err: %v", rerr)
}

// --- helpers ---

// mustPipeConn returns a connected loopback TCP pair (real *net.TCPConn, which
// splice/SendConn require).
func mustPipeConn(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		ch <- res{c, err}
	}()
	dialed, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatalf("accept: %v", r.err)
	}
	return r.c, dialed
}

// mustSocketpairConn builds a connected STREAM socketpair, returning one end as
// a raw fd (the "client fd" handleIngressConn adopts) and the other as a
// *net.Conn the test drives. Mirrors how SendConn passes an accepted client fd.
func mustSocketpairConn(t *testing.T) (int, net.Conn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	// fds[1] becomes the test-driven conn; fds[0] is handed to handleIngressConn.
	f := os.NewFile(uintptr(fds[1]), "test-client-peer")
	conn, err := net.FileConn(f)
	_ = f.Close()
	if err != nil {
		syscall.Close(fds[0])
		t.Fatalf("FileConn: %v", err)
	}
	return fds[0], conn
}

func mustRead(t *testing.T, c net.Conn, n int) string {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, n)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("read %d bytes: %v", n, err)
	}
	return string(buf)
}

// A passthrough connection with no traffic in either direction must be closed:
// nothing else on this path can ever close it (the mux left when the fd was
// passed, the L7 is not in the path).
func TestSplice_ClosesIdleConnection(t *testing.T) {
	clientSide, clientPeer := net.Pipe()
	backendSide, backendPeer := net.Pipe()
	defer clientPeer.Close()
	defer backendPeer.Close()

	done := make(chan struct{})
	go func() {
		spliceWithIdle(clientSide, backendSide, 50*time.Millisecond)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("splice never returned on an idle connection — the idle deadline did not fire")
	}
}

// A long-lived connection that keeps transferring must NOT be cut: the deadline
// is an IDLE one, rearmed on activity, not an absolute cap. Traffic runs for
// well past the timeout in one direction only, which must be enough to keep
// both ends alive.
func TestSplice_KeepsActiveConnectionAlive(t *testing.T) {
	clientSide, clientPeer := net.Pipe()
	backendSide, backendPeer := net.Pipe()

	const idle = 100 * time.Millisecond
	spliced := make(chan struct{})
	go func() {
		spliceWithIdle(clientSide, backendSide, idle)
		close(spliced)
	}()

	// Drain whatever reaches the backend so the copy never blocks on a full pipe.
	go func() { _, _ = io.Copy(io.Discard, backendPeer) }()

	// Write for 5x the idle timeout, one chunk per fifth of it.
	deadline := time.Now().Add(5 * idle)
	for time.Now().Before(deadline) {
		if _, err := clientPeer.Write([]byte("ping")); err != nil {
			t.Fatalf("active connection was cut mid-transfer: %v", err)
		}
		time.Sleep(idle / 5)
	}

	select {
	case <-spliced:
		t.Fatal("splice returned while traffic was still flowing — the deadline is absolute, not idle")
	default:
	}

	clientPeer.Close()
	backendPeer.Close()
	<-spliced
}

// TestHandleIngressConnProxyProtocol checks the backend receives the PROXY v2
// header before the client's own bytes when the request asks for it.
func TestHandleIngressConnProxyProtocol(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan []byte, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 16+5)
		if _, err := io.ReadFull(conn, buf); err != nil {
			got <- nil
			return
		}
		got <- buf
	}()
	dial := func(string, int) (net.Conn, error) { return net.Dial("tcp", ln.Addr().String()) }

	clientFD, clientEnd := mustSocketpairConn(t)
	defer clientEnd.Close()
	go handleIngressConn(clientFD, ingressRequest{Service: "mail", Port: 25, ProxyProtocol: true}, dial)
	if _, err := clientEnd.Write([]byte("EHLO ")); err != nil {
		t.Fatal(err)
	}
	buf := <-got
	if buf == nil {
		t.Fatal("backend read failed")
	}
	// The socketpair client is not TCP, so the worker sends a LOCAL header.
	wantHdr := append(append([]byte(nil), proxyV2Signature...), 0x20, 0x00, 0x00, 0x00)
	if string(buf[:16]) != string(wantHdr) {
		t.Fatalf("header = % x, want % x", buf[:16], wantHdr)
	}
	if string(buf[16:]) != "EHLO " {
		t.Fatalf("payload after header = %q", buf[16:])
	}
}
