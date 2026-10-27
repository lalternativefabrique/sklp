package space

import (
	"fmt"
	"net"
	"os"
	"sync"
)

// ingressBindHost is the interface the host ingress listeners bind. Loopback by
// default (safest: only this host reaches the space port). A future flag could
// widen it to "0.0.0.0" for LAN exposure; kept a single constant so the policy
// lives in one place.
const ingressBindHost = "127.0.0.1"

// PortMapping is one host->space ingress: HostPort on BindHost (loopback when
// empty) is proxied to ContainerPort on a Ready replica of a service
// (round-robin). It mirrors the deploy-layer mapping so the launcher consumes
// a parsed value without depending on the deploy package (which imports runtime).
type PortMapping struct {
	BindHost      string
	HostPort      int
	ContainerPort int
	ProxyProtocol bool
	UDP           bool
}

// ACLEdge is one declared depends_on edge: caller service name -> target
// service name. Mirrors spacenet.ACLEdge for the same reason PortMapping
// mirrors the deploy-layer mapping — this file has no linux build tag, so it
// must not import spacenet (linux-only), but SetACLEdges' signature still
// needs a type here that compiles on every platform.
type ACLEdge struct {
	From string
	To   string
}

// hostIngress owns the host-side TCP listeners that front a space's exposed
// services. Each listener binds 127.0.0.1:<hostPort> and, per accepted
// connection, hands the socket to the space worker (cnet.sendConn), which
// dials the next replica IN THE SPACE NETNS and splices. One hostIngress is
// shared by a space; listeners are opened as services become Ready and all
// closed on teardown. Safe for concurrent OpenService/Close.
type hostIngress struct {
	cnet *spaceNet

	mu        sync.Mutex
	listeners map[string][]net.Listener      // service name -> its host listeners
	attempted map[string]map[int]bool        // service name -> host ports already tried (bound or failed)
	current   map[string]map[int]PortMapping // service name -> host port -> mapping its connections follow
	closed    bool
}

// newHostIngress builds an ingress controller bound to a space network. A nil
// cnet (standalone launcher / non-linux) yields a controller whose OpenService
// is a no-op, so callers need no build-tag branching.
func newHostIngress(cnet *spaceNet) *hostIngress {
	return &hostIngress{
		cnet:      cnet,
		listeners: map[string][]net.Listener{},
		attempted: map[string]map[int]bool{},
		current:   map[string]map[int]PortMapping{},
	}
}

// OpenService binds a host listener for each of service's port mappings and
// starts proxying. Idempotent per PORT, not per service: a second call with the
// same mappings (e.g. a second replica becoming Ready) is a no-op, but a call
// bringing a NEW host port (the gateway allocating a backend for a service
// whose declared ingress is already open) binds it — per-service idempotence
// silently swallowed exactly that mapping, leaving the gateway routing to a
// port nobody listened on. A bind failure (EADDRINUSE, privileged port) is
// logged, skipped, and never re-attempted — it never aborts the deploy; the
// other mappings still come up. The feedback line (where to curl) is printed
// once per successfully-bound mapping.
func (h *hostIngress) OpenService(serviceName string, mappings []PortMapping) {
	if h == nil || h.cnet == nil || len(mappings) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	tried := h.attempted[serviceName]
	if tried == nil {
		tried = map[int]bool{}
		h.attempted[serviceName] = tried
	}
	cur := h.current[serviceName]
	if cur == nil {
		cur = map[int]PortMapping{}
		h.current[serviceName] = cur
	}

	for _, m := range mappings {
		cur[m.HostPort] = m
		if tried[m.HostPort] {
			continue
		}
		tried[m.HostPort] = true
		bindHost := m.BindHost
		if bindHost == "" {
			bindHost = ingressBindHost
		}
		addr := net.JoinHostPort(bindHost, fmt.Sprintf("%d", m.HostPort))
		if m.UDP {
			h.openUDP(serviceName, m, addr)
			continue
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			// Best-effort: a taken/privileged port must not kill the deploy. Host
			// ports are necessarily global (two processes can't bind one port), so
			// a fixed host port already taken by ANOTHER SPACE lands here — the
			// service stays reachable east-west by DNS, just not on this host port.
			// Surface the likely cause so it isn't mistaken for a system-port clash.
			fmt.Fprintf(os.Stderr, "[sklp deploy] ingress: host port %d is already in use (another space, "+
				"or a system process?) — %q stays reachable in-space by name, but not on %s (%v)\n",
				m.HostPort, serviceName, addr, err)
			continue
		}
		h.listeners[serviceName] = append(h.listeners[serviceName], ln)
		go h.accept(serviceName, m.HostPort, ln)
		fmt.Printf("↥ %s → http://%s\n", serviceName, addr)
	}
}

// openUDP binds addr and hands the socket to the space worker, which relays
// its datagrams for the rest of the space's life. The worker owns the only
// remaining copy, so the port is released when the space is torn down.
func (h *hostIngress) openUDP(serviceName string, m PortMapping, addr string) {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[sklp deploy] ingress: udp port %d is already in use — %q is not reachable on %s/udp (%v)\n",
			m.HostPort, serviceName, addr, err)
		return
	}
	if err := h.cnet.sendPacketConn(serviceName, m.ContainerPort, pc.(*net.UDPConn)); err != nil {
		fmt.Fprintf(os.Stderr, "[sklp deploy] ingress: %q: %v\n", serviceName, err)
		return
	}
	fmt.Printf("↥ %s → %s/udp\n", serviceName, addr)
}

// accept loops on ln, handing each connection to the space worker to splice
// to a replica dialed in the space netns. It returns when ln is closed
// (teardown), so there is no goroutine leak past the listener's life.
func (h *hostIngress) accept(serviceName string, hostPort int, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed on teardown
		}
		m := h.mapping(serviceName, hostPort)
		// sendConn takes ownership of conn (passes the fd to the worker and
		// closes the host copy). On failure it still closes conn, so a backend
		// with no Ready replica simply drops the client.
		if serr := h.cnet.sendConn(serviceName, m.ContainerPort, m.ProxyProtocol, conn); serr != nil {
			fmt.Fprintf(os.Stderr, "[sklp deploy] ingress: %q: %v\n", serviceName, serr)
		}
	}
}

// mapping is the mapping a new connection on hostPort follows: the latest one
// OpenService was given, so a re-applied spec retargets an open listener.
func (h *hostIngress) mapping(serviceName string, hostPort int) PortMapping {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.current[serviceName][hostPort]
}

// Close shuts every host listener down (idempotent). Accept loops return when
// their listener closes; in-flight splices live in the worker and end when the
// space netns is torn down by spaceNet.close().
func (h *hostIngress) Close() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for _, lns := range h.listeners {
		for _, ln := range lns {
			_ = ln.Close()
		}
	}
	h.listeners = map[string][]net.Listener{}
}
