package netctrl

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/lalternativefabrique/sklp/internal/netaccept"
)

// tcpRouter owns one net.Listener PER BIND ADDRESS per registered service
// (e.g. one on the controller's local IP, one more on its mesh IP once
// joined), splicing every accepted connection straight to the real backend
// with no inspection of the traffic itself. A remote node's caller reaches
// the exact same splice logic a local caller would, just via the mesh
// listener instead of the local one — same port on both.
//
// The listener binds the service's DECLARED port, so a caller dials the port
// the manifest names (postgres:5432, nats:4222) with nothing to discover —
// the same property that makes the HTTP proxy usable by name alone, since
// there the port is always 80. Two services on one node cannot declare the
// same port: they share the controller's IP, so the second bind fails and is
// reported, leaving that service reachable by HTTP and in-space as before.
type tcpRouter struct {
	ips []net.IP

	mu        sync.Mutex
	listeners map[string][]net.Listener // key: "<space>.<service>"
	ports     map[string]int            // key: "<space>.<service>" -> declared port
	// refused remembers the port a service could not bind, so the Reconciler
	// — which re-Puts every service on every tick — reports the clash once
	// instead of retrying a bind that cannot succeed several times a minute
	// forever. Cleared by Unregister, so a service whose port frees up is
	// picked up again the next time it is registered.
	refused map[string]int // key: "<space>.<service>" -> port that failed to bind

	// onPorts is told the full set of ports listened on after each change.
	onPorts func([]int)
}

func newTCPRouter(ips ...net.IP) *tcpRouter {
	return &tcpRouter{
		ips:       ips,
		listeners: map[string][]net.Listener{},
		ports:     map[string]int{},
		refused:   map[string]int{},
	}
}

// Register opens a dedicated listener (on every configured bind address) for
// space/service on port, the service's declared port, if one doesn't already
// exist. Idempotent: a second Register for the same service is a no-op.
func (t *tcpRouter) Register(space, service string, port int, backend Endpoint) (err error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if port <= 0 || port > 65535 {
		return fmt.Errorf("netctrl: tcp register %s: declared port %d out of range", key(space, service), port)
	}

	k := key(space, service)
	if _, ok := t.ports[k]; ok {
		return nil
	}
	if p, ok := t.refused[k]; ok && p == port {
		return nil
	}

	var lns []net.Listener
	for _, ip := range t.ips {
		ln, err := net.Listen("tcp", net.JoinHostPort(ip.String(), strconv.Itoa(port)))
		if err != nil {
			for _, opened := range lns {
				opened.Close()
			}
			t.refused[k] = port
			return fmt.Errorf("netctrl: tcp listen for %s on port %d: %w", k, port, err)
		}
		lns = append(lns, ln)
	}
	t.listeners[k] = lns
	t.ports[k] = port

	for _, ln := range lns {
		go acceptSplice(ln, backend)
	}
	t.notifyPortsLocked()
	return nil
}

// notifyPortsLocked hands the listened port set to onPorts. Called with mu
// held; onPorts must not call back into the router.
func (t *tcpRouter) notifyPortsLocked() {
	if t.onPorts == nil {
		return
	}
	t.onPorts(t.portsLocked())
}

func (t *tcpRouter) portsLocked() []int {
	out := make([]int, 0, len(t.ports))
	for _, p := range t.ports {
		out = append(out, p)
	}
	return out
}

// Unregister closes space/service's dedicated listeners, if any.
func (t *tcpRouter) Unregister(space, service string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	k := key(space, service)
	delete(t.refused, k)
	if lns, ok := t.listeners[k]; ok {
		for _, ln := range lns {
			ln.Close()
		}
		delete(t.listeners, k)
		delete(t.ports, k)
		t.notifyPortsLocked()
	}
}

func (t *tcpRouter) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, lns := range t.listeners {
		for _, ln := range lns {
			ln.Close()
		}
		delete(t.listeners, k)
		delete(t.ports, k)
	}
}

func acceptSplice(ln net.Listener, backend Endpoint) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			// Not every error is a closed listener: running out of descriptors
			// lands here too, and returning would stop routing this service
			// permanently over a condition that clears on its own.
			if delay, retry := netaccept.Temporary(err); retry {
				time.Sleep(delay)
				continue
			}
			return // listener closed: Unregister/close, not a real failure
		}
		go splice(conn, backend)
	}
}

func splice(client net.Conn, backend Endpoint) {
	defer client.Close()
	upstream, err := net.Dial("tcp", net.JoinHostPort(backend.IP, strconv.Itoa(backend.Port)))
	if err != nil {
		return
	}
	defer upstream.Close()

	done := make(chan struct{}, 2)
	go func() { io.Copy(upstream, client); done <- struct{}{} }()
	go func() { io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}
