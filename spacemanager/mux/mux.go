//go:build linux

// Package mux is the host gateway front door for the ingress-per-space model. It
// binds one host port (e.g. :9080), peeks the HTTP Host header of each accepted
// connection WITHOUT consuming it (MSG_PEEK), looks up which space owns that
// host, and fd-passes the still-intact connection to that space's worker over
// its filesystem ingress socket. The worker splices it to the injected
// "sklp-ingress" container in the space netns, which routes by space DNS.
//
// This is the rootless replacement for "the node routes to the pod network":
// the space bridge is not reachable from the host (separate userns/netns), so
// the mux reaches it by passing the client fd across the process boundary rather
// than by dialing a routable backend IP.
package mux

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lalternativefabrique/sklp/space/spacenet"
	"github.com/lalternativefabrique/sklp/spacemanager"

	"github.com/lalternativefabrique/sklp/internal/netaccept"
)

// peekTimeout bounds how long the mux waits for enough bytes to read the Host
// header before giving up on a connection (a slow-loris / non-HTTP client).
const peekTimeout = 5 * time.Second

// Mux is the host front door. It owns the listener, the host->space routing
// table, and a per-space ingress client cache. Safe for concurrent use.
type Mux struct {
	mu      sync.RWMutex
	routes  map[string]Target                  // lower-case host -> target service
	clients map[string]*spacenet.IngressClient // space -> reused ingress client

	httpLn net.Listener // :9080 — routes by HTTP Host header
	tlsLn  net.Listener // :9443 — routes by TLS SNI (passthrough)
}

// New builds an empty mux. Register hosts with SetRoutes, then Serve.
func New() *Mux {
	return &Mux{
		routes:  map[string]Target{},
		clients: map[string]*spacenet.IngressClient{},
	}
}

// SetRoutes replaces the host->target table wholesale (the desired set). Hosts
// are lower-cased. A cached client for a space no longer referenced by any route
// is dropped so its FS socket connection is released.
func (m *Mux) SetRoutes(hostToTarget map[string]Target) {
	norm := make(map[string]Target, len(hostToTarget))
	live := make(map[string]bool, len(hostToTarget))
	for h, t := range hostToTarget {
		norm[strings.ToLower(h)] = t
		live[t.Space] = true
	}
	m.mu.Lock()
	m.routes = norm
	for space, cl := range m.clients {
		if !live[space] {
			_ = cl.Close()
			delete(m.clients, space)
		}
	}
	m.mu.Unlock()
}

// Listen binds the HTTP front door (e.g. ":9080"). Serve must be called after.
func (m *Mux) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("mux listen %q: %w", addr, err)
	}
	m.httpLn = ln
	return nil
}

// ListenTLS binds the HTTPS front door (e.g. ":9443"), routed by TLS SNI
// (passthrough — the mux never terminates TLS). Optional: omit it for an
// HTTP-only host. Serve drives both listeners.
func (m *Mux) ListenTLS(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("mux listen tls %q: %w", addr, err)
	}
	m.tlsLn = ln
	return nil
}

// Addr reports the bound HTTP address (useful when Listen used :0 in tests).
func (m *Mux) Addr() net.Addr {
	if m.httpLn == nil {
		return nil
	}
	return m.httpLn.Addr()
}

// AddrTLS reports the bound HTTPS address (nil when ListenTLS was not called).
func (m *Mux) AddrTLS() net.Addr {
	if m.tlsLn == nil {
		return nil
	}
	return m.tlsLn.Addr()
}

// Serve accepts on every bound listener until they close, routing HTTP by Host
// and HTTPS by SNI. It blocks until both accept loops end and returns the first
// non-nil error (nil on a clean Close).
func (m *Mux) Serve() error {
	errc := make(chan error, 2)
	live := 0
	if m.httpLn != nil {
		live++
		go func() { errc <- m.acceptLoop(m.httpLn, peekHost, false) }()
	}
	if m.tlsLn != nil {
		live++
		go func() { errc <- m.acceptLoop(m.tlsLn, peekSNI, true) }()
	}
	var first error
	for i := 0; i < live; i++ {
		if err := <-errc; err != nil && first == nil {
			first = err
		}
	}
	return first
}

// acceptLoop accepts connections on ln and dispatches each to handle with the
// given peeker (Host for HTTP, SNI for HTTPS). tls marks the HTTPS listener so
// handle routes to the space's native L7 TLS endpoint.
func (m *Mux) acceptLoop(ln net.Listener, peek peeker, tls bool) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			// Out of descriptors is temporary — one frees up as soon as any
			// connection closes. Returning here would end the loop for good,
			// taking the front door of EVERY space down until a human restarts
			// the mux, over a condition that resolves itself in milliseconds.
			if delay, retry := netaccept.Temporary(err); retry {
				time.Sleep(delay)
				continue
			}
			return err
		}
		go m.handle(conn, peek, tls)
	}
}

// Close stops every listener and drops every cached ingress client.
func (m *Mux) Close() error {
	var err error
	if m.httpLn != nil {
		if e := m.httpLn.Close(); e != nil {
			err = e
		}
	}
	if m.tlsLn != nil {
		if e := m.tlsLn.Close(); e != nil && err == nil {
			err = e
		}
	}
	m.mu.Lock()
	for s, cl := range m.clients {
		_ = cl.Close()
		delete(m.clients, s)
	}
	m.mu.Unlock()
	return err
}

// peeker extracts the routing key (Host or SNI) from a connection without
// consuming its bytes (MSG_PEEK), so the fd-passed stream stays intact.
type peeker func(*net.TCPConn) (string, error)

// handle peeks the routing key (via peek), resolves the owning space, and
// fd-passes the connection (still intact — peek did not consume) to that space's
// worker for the netns splice. For HTTPS (tls=true) the bytes are the TLS
// handshake, passed through untouched to the space's native L7 TLS endpoint
// which TERMINATES TLS; the per-route service/port is irrelevant there since the
// in-netns ingress re-routes by Host/path after decrypting.
func (m *Mux) handle(conn net.Conn, peek peeker, tls bool) {
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		_ = conn.Close()
		return
	}
	host, err := peek(tcp)
	if err != nil {
		_ = conn.Close()
		return
	}
	target, ok := m.lookup(host)
	if !ok {
		// Unknown host: no route. Close (the HTTP 421-equivalent for a raw mux).
		_ = conn.Close()
		return
	}
	if tls {
		// HTTPS: pass the whole handshake to the space's native L7 TLS endpoint,
		// which terminates TLS and re-routes by Host/path. Only the SPACE matters.
		target = Target{Space: target.Space, Service: spacemanager.L7TLSServiceName, Port: spacemanager.L7TLSPort}
	}
	client, err := m.clientFor(target.Space)
	if err != nil {
		_ = conn.Close()
		return
	}
	// SendConn closes conn (the worker holds its own dup). The peeked bytes are
	// still in the socket buffer, so the backend sees the full request. The
	// worker dials target.Service:target.Port in the netns by space DNS — for a
	// plain Host route that's the service DIRECTLY (no ingress hop).
	if serr := client.SendConn(target.Service, target.Port, conn); serr != nil {
		// A dead client (worker restarted) — drop it so the next connection
		// re-dials. conn is already closed by SendConn.
		m.dropClient(target.Space)
	}
}

// lookup resolves a Host header (may carry :port) to its target.
func (m *Mux) lookup(hostHeader string) (Target, bool) {
	h := strings.ToLower(hostHeader)
	if i := strings.IndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	m.mu.RLock()
	t, ok := m.routes[h]
	m.mu.RUnlock()
	return t, ok
}

// clientFor returns the (cached) ingress client for space, dialing its FS socket
// on first use. The path is derived from the space name alone, so the mux reaches
// a space owned by a different supervisor process.
func (m *Mux) clientFor(space string) (*spacenet.IngressClient, error) {
	m.mu.RLock()
	cl := m.clients[space]
	m.mu.RUnlock()
	if cl != nil {
		return cl, nil
	}
	cl, err := spacenet.DialIngress(spacenet.IngressSockPath(space))
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	// Re-check: a racing caller may have dialed first.
	if existing := m.clients[space]; existing != nil {
		m.mu.Unlock()
		_ = cl.Close()
		return existing, nil
	}
	m.clients[space] = cl
	m.mu.Unlock()
	return cl, nil
}

// dropClient removes a space's cached client (e.g. after a send failure) so the
// next connection re-dials a fresh FS socket.
func (m *Mux) dropClient(space string) {
	m.mu.Lock()
	if cl := m.clients[space]; cl != nil {
		_ = cl.Close()
		delete(m.clients, space)
	}
	m.mu.Unlock()
}

// peekHost reads the HTTP Host header WITHOUT consuming the bytes (MSG_PEEK), so
// the connection fd-passed to the worker still carries the full request. It
// loops MSG_PEEK with a growing window until net/http can parse the request line
// + headers, bounded by peekTimeout.
func peekHost(tcp *net.TCPConn) (string, error) {
	_ = tcp.SetReadDeadline(time.Now().Add(peekTimeout))
	defer tcp.SetReadDeadline(time.Time{})

	raw, err := tcp.SyscallConn()
	if err != nil {
		return "", err
	}

	buf := make([]byte, 4096)
	var host string
	var parseErr error
	ctrlErr := raw.Read(func(fd uintptr) bool {
		n, _, rerr := syscall.Recvfrom(int(fd), buf, syscall.MSG_PEEK)
		if rerr == syscall.EAGAIN {
			return false // not enough data yet; runtime re-polls
		}
		if rerr != nil {
			parseErr = rerr
			return true
		}
		h, perr := hostFromRequest(buf[:n])
		if perr != nil {
			// Need more bytes? If the buffer is full we cannot grow within one
			// peek call; treat a short read as incomplete and wait for more.
			if n == len(buf) {
				parseErr = perr
				return true
			}
			return false // incomplete request; peek again when more arrives
		}
		host = h
		return true
	})
	if ctrlErr != nil {
		return "", ctrlErr
	}
	if parseErr != nil {
		return "", parseErr
	}
	if host == "" {
		return "", fmt.Errorf("mux: no Host header")
	}
	return host, nil
}

// hostFromRequest parses the Host out of peeked HTTP bytes. Returns an error if
// the request is incomplete (caller peeks more) or has no Host.
func hostFromRequest(peeked []byte) (string, error) {
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(peeked)))
	if err != nil {
		return "", err
	}
	if req.Host == "" {
		return "", fmt.Errorf("mux: request has no Host header")
	}
	return req.Host, nil
}
