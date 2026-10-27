package space

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

// hostMuxPortBase is the bottom of the unprivileged port range the per-worktree
// mux port is derived into. High port (not :80) so the bind needs no root: a
// normal dev runs `sklp dev` unprivileged. macOS-friendly: a loopback high port
// on 127.0.0.1 binds identically on Linux and macOS, unlike a 127.0.0.x alias.
const hostMuxPortBase = 8080

// hostMuxPortSpan caps the derived range [8080, 8080+span). One port per live
// `sklp dev` (worktree). The derived port is STABLE per worktree (bookmarkable);
// a fallback walks +1 on collision so two worktrees hashing to the same port
// still both come up.
const hostMuxPortSpan = 1000

// hostMuxMaxProbe bounds the +1 fallback walk so a saturated range fails fast
// rather than spinning. Far above any realistic count of parallel dev stacks.
const hostMuxMaxProbe = 64

// hostRouter is the splice surface a registered route needs: hand the accepted
// client connection (with the already-peeked bytes replayed) to the space, which
// dials <service>:<port> in its netns and splices. *Space implements it
// (SendConnReplay); a non-linux/standalone space yields an error so the route is
// simply unreachable rather than panicking.
type hostRouter interface {
	SendConnReplay(service string, port int, client net.Conn, prefix []byte) error
}

// route is one hostname -> (space, service, containerPort) entry. service is the
// space DNS name SendConn resolves; port is where the service listens INSIDE the
// container (parsed from its ready probe).
type route struct {
	router  hostRouter
	service string
	port    int
}

// hostMux is the per-PROCESS mux that owns one per-worktree loopback port and
// demuxes accepted connections to a registered route by HTTP Host header. It is
// ref-counted across the stacks of one `sklp dev` (acquire/release like
// controller.gatewayManager): the listener opens on the first acquire and closes
// on the last release. Two `sklp dev` worktrees are two processes with two
// derived ports — both stay host-reachable at once, no shared bind to fight over.
type hostMux struct {
	basePort int // derived per-worktree start port; bind walks +1 from here
	mu       sync.Mutex
	ln       net.Listener
	port     int              // the actually-bound port (basePort or a +1 fallback)
	routes   map[string]route // lower-cased hostname -> route
	missed   map[string]bool  // hosts already warned about (dedup the no-route log)
	closed   bool
}

// newHostMux builds a mux that will bind on 127.0.0.1, starting at basePort and
// walking +1 on collision. basePort==0 lets the kernel pick an ephemeral port
// (tests, to avoid colliding with a real dev mux).
func newHostMux(basePort int) *hostMux {
	return &hostMux{basePort: basePort, routes: map[string]route{}, missed: map[string]bool{}}
}

// start binds the loopback listener, walking from basePort by +1 until a free
// port is found (so two worktrees hashing to the same base still both come up),
// and serves until close. An EADDRINUSE walks on; any other error, or a fully
// saturated range, is returned so the manager does not memoise a dead mux and
// the caller surfaces it without aborting dev (hostnames just won't resolve,
// services stay reachable in-space by name).
func (m *hostMux) start() error {
	ln, port, err := listenLoopbackFrom(m.basePort)
	if err != nil {
		return fmt.Errorf("host mux: %w", err)
	}
	m.mu.Lock()
	m.ln = ln
	m.port = port
	m.mu.Unlock()
	go m.serve(ln)
	return nil
}

// boundPort returns the port the mux actually bound, for the printed URL.
func (m *hostMux) boundPort() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.port
}

// listenLoopbackFrom binds 127.0.0.1 starting at base, walking +1 past an
// in-use port up to hostMuxMaxProbe attempts. base==0 means an ephemeral port
// (single attempt). Returns the listener and the bound port.
func listenLoopbackFrom(base int) (net.Listener, int, error) {
	if base == 0 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, 0, err
		}
		return ln, ln.Addr().(*net.TCPAddr).Port, nil
	}
	for i := 0; i < hostMuxMaxProbe; i++ {
		port := base + i
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			return ln, port, nil
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			return nil, 0, fmt.Errorf("bind 127.0.0.1:%d: %w", port, err)
		}
	}
	return nil, 0, fmt.Errorf("no free loopback port in [%d,%d)", base, base+hostMuxMaxProbe)
}

// HostMuxPortFor derives the stable per-worktree base port from a scope id (the
// devScopeID hash). Same worktree → same port (bookmarkable); the bind-time +1
// fallback resolves the rare cross-worktree hash collision. Empty scope falls
// back to the base port.
func HostMuxPortFor(scope string) int {
	if scope == "" {
		return hostMuxPortBase
	}
	var acc uint32
	for i := 0; i < len(scope); i++ {
		acc = acc*31 + uint32(scope[i])
	}
	return hostMuxPortBase + int(acc%hostMuxPortSpan)
}

// register maps host -> route. Idempotent: a re-register (second replica Ready)
// overwrites with the same target. Host is normalised to lower-case, no port.
func (m *hostMux) register(host string, r route) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	h := strings.ToLower(host)
	m.routes[h] = r
	delete(m.missed, h) // re-arm the no-route warning if this host is seen again
}

// firstMiss reports whether host has not been warned about yet, recording it so
// subsequent unknown-host hits stay silent. Normalised (lower-case, no port) the
// same way as lookup/register so "Core.X:80" and "core.x" dedup as one key.
func (m *hostMux) firstMiss(host string) bool {
	h := strings.ToLower(host)
	if i := strings.IndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.missed[h] {
		return false
	}
	m.missed[h] = true
	return true
}

// unregister drops a hostname (service stopped / space torn down). Idempotent.
func (m *hostMux) unregister(host string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.routes, strings.ToLower(host))
}

// lookup resolves a Host header (may carry :port) to its route.
func (m *hostMux) lookup(hostHeader string) (route, bool) {
	h := strings.ToLower(hostHeader)
	if i := strings.IndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.routes[h]
	return r, ok
}

// serve accepts and dispatches each connection in its own goroutine.
func (m *hostMux) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed on teardown
		}
		go m.handle(conn)
	}
}

// peekTimeout caps how long we wait for the request line + headers before giving
// up: a client that connects but sends nothing must not pin a goroutine.
const peekTimeout = 10 * time.Second

// handle peeks the HTTP Host header, looks up the route, and hands the connection
// (with the peeked bytes replayed) to the owning space for the netns splice. A
// missing Host / unknown host / splice error closes the connection.
func (m *hostMux) handle(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(peekTimeout))
	br := bufio.NewReader(conn)
	host, buffered, err := peekHostHeader(br)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		_ = conn.Close()
		return
	}
	r, ok := m.lookup(host)
	if !ok {
		// Warn once per unknown host: a stale browser tab / typo would otherwise
		// spam stderr on every favicon, preconnect, and reload.
		if m.firstMiss(host) {
			fmt.Fprintf(os.Stderr, "[sklp dev] host mux: no route for %q\n", host)
		}
		_ = conn.Close()
		return
	}
	if serr := r.router.SendConnReplay(r.service, r.port, conn, buffered); serr != nil {
		fmt.Fprintf(os.Stderr, "[sklp dev] host mux: %q: %v\n", host, serr)
		_ = conn.Close()
	}
}

// close stops the listener (idempotent). In-flight splices live in the space
// worker and end when the space netns is torn down.
func (m *hostMux) close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.closed = true
	if m.ln != nil {
		_ = m.ln.Close()
	}
	m.routes = map[string]route{}
}

// peekHostHeader reads just enough of the request to learn the Host header,
// returning the header value and ALL bytes consumed from br so the caller can
// replay them to the backend (the request must reach the service intact). It
// reuses net/http's request parser to stay spec-correct (folded headers, the
// absolute-form request target, etc.).
func peekHostHeader(br *bufio.Reader) (host string, buffered []byte, err error) {
	var cap bytes.Buffer
	tee := bufio.NewReader(io.TeeReader(br, &cap))
	req, err := http.ReadRequest(tee)
	if err != nil {
		return "", nil, err
	}
	if req.Host == "" {
		return "", nil, fmt.Errorf("request has no Host header")
	}
	// cap holds every byte ReadRequest pulled from br (request line + headers,
	// plus any pipelined body bytes already buffered). Replay them verbatim.
	return req.Host, cap.Bytes(), nil
}
