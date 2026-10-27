package netctrl

import (
	"fmt"
	"net"
	"strconv"
)

// Controller wires the Registry to a DNS answerer, an HTTP reverse proxy,
// and per-service TCP listeners — one process, external to every Space,
// mediating every cross-Space call. IP is the single fixed local address
// every Space is routed to via a /32 exception through its egress
// blackhole (see space/spacenet/allowcontroller_linux.go). When
// mesh is joined, the HTTP proxy and TCP listeners ALSO bind on MeshIP, so
// a remote node's caller (resolved via DNS to MeshIP directly, never
// relayed through its own controller) lands on the exact same routing
// logic as a local caller.
type Controller struct {
	IP     net.IP
	MeshIP net.IP // nil when mesh is not joined
	reg    *Registry
	dns    *dnsServer
	http   *httpProxy
	tcp    *tcpRouter
	peers  *PeerCache
}

// Options lets a caller override the DNS/HTTP ports Start binds — tests use
// ephemeral ports since :53/:80 need root; production sklet wiring uses the
// zero-value default (53/80), matching spaceDNS's own gateway convention so
// Space resolv.conf wiring stays uniform. MeshIP, when set, is also bound
// by the HTTP proxy and TCP listeners (see Controller's doc comment).
// SelfPeerID is this node's own meshauthority peer ID, used to skip its own
// entries in the peer cache defensively.
type Options struct {
	DNSPort    int
	HTTPPort   int
	MeshIP     net.IP
	SelfPeerID string
}

func (o Options) dnsPort() int {
	if o.DNSPort != 0 {
		return o.DNSPort
	}
	return 53
}

func (o Options) httpPort() int {
	if o.HTTPPort != 0 {
		return o.HTTPPort
	}
	return 80
}

// Start binds the DNS answerer on ip, and the HTTP proxy + TCP router on ip
// (plus opts.MeshIP too, when set — see Controller's doc comment).
func Start(ip net.IP, opts ...Options) (*Controller, error) {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	reg := NewRegistry()
	peers := NewPeerCache(o.SelfPeerID)

	d, err := newDNSServer(reg, ip, net.JoinHostPort(ip.String(), strconv.Itoa(o.dnsPort())), peers)
	if err != nil {
		return nil, err
	}

	httpAddrs := []string{net.JoinHostPort(ip.String(), strconv.Itoa(o.httpPort()))}
	tcpIPs := []net.IP{ip}
	if o.MeshIP != nil {
		httpAddrs = append(httpAddrs, net.JoinHostPort(o.MeshIP.String(), strconv.Itoa(o.httpPort())))
		tcpIPs = append(tcpIPs, o.MeshIP)
	}

	p, err := newHTTPProxy(reg, httpAddrs...)
	if err != nil {
		d.close()
		return nil, fmt.Errorf("netctrl: start http proxy: %w", err)
	}

	return &Controller{
		IP:     ip,
		MeshIP: o.MeshIP,
		reg:    reg,
		dns:    d,
		http:   p,
		tcp:    newTCPRouter(tcpIPs...),
		peers:  peers,
	}, nil
}

// OnServedPorts registers f to learn every TCP port the controller serves on
// its addresses — DNS, the HTTP proxy, and each service's declared port — now
// and after every change. f runs with the router locked and must not call
// back into the controller.
func (c *Controller) OnServedPorts(dnsPort, httpPort int, f func([]int)) {
	c.tcp.mu.Lock()
	defer c.tcp.mu.Unlock()
	c.tcp.onPorts = func(ports []int) { f(append([]int{dnsPort, httpPort}, ports...)) }
	c.tcp.notifyPortsLocked()
}

// Peers returns the controller's peer cache, refreshed by the Reconciler
// from meshauthority's peer list on its own polling cadence.
func (c *Controller) Peers() *PeerCache {
	return c.peers
}

// Put registers a service reachable at backendIP:backendPort. DNS + the
// reverse proxy make it reachable over HTTP at <service>.<space>.internal,
// and a dedicated TCP listener on declaredPort makes the same service
// reachable as raw TCP too (Postgres, Redis, non-HTTP gRPC) without the
// caller having to declare which kind of traffic it is.
//
// declaredPort is the port the service's manifest names, which is the port
// callers dial — backendIP:backendPort is where the controller splices to
// and is an implementation detail no caller ever sees.
//
// Primitive (string, int) signature — rather than taking Endpoint directly —
// so callers outside this package (e.g. space.NetController) can implement
// or satisfy this shape without importing netctrl and its NATS-adjacent
// dependency tree.
func (c *Controller) Put(space, service, backendIP string, backendPort, declaredPort int) error {
	ep := Endpoint{IP: backendIP, Port: backendPort}
	c.reg.Put(space, service, ep)
	return c.tcp.Register(space, service, declaredPort, ep)
}

// PutHTTP registers a service for DNS and the HTTP proxy only, with no raw
// TCP listener: the space gateway's in-netns port is the same in every space,
// so a per-service TCP port on the controller would collide across spaces.
func (c *Controller) PutHTTP(space, service, backendIP string, backendPort int) {
	c.reg.Put(space, service, Endpoint{IP: backendIP, Port: backendPort})
}

// PutTCP opens a raw TCP listener on port splicing to backendIP:backendPort,
// with no DNS or HTTP registration.
func (c *Controller) PutTCP(space, name, backendIP string, backendPort, port int) error {
	return c.tcp.Register(space, name, port, Endpoint{IP: backendIP, Port: backendPort})
}

// Delete deregisters one service: DNS answers NXDOMAIN, the HTTP proxy
// 502s, and its dedicated TCP listener closes.
func (c *Controller) Delete(space, service string) {
	c.reg.Delete(space, service)
	c.tcp.Unregister(space, service)
}

// DeleteAll deregisters every service belonging to a Space, on full Space
// teardown.
func (c *Controller) DeleteAll(space string) {
	c.reg.DeleteAll(space)
	// tcpRouter has no bulk-by-space index in v1; Space teardown already
	// walks its own service list and calls Delete per service, so this is
	// only a registry-level safety net against a caller that doesn't.
}

func (c *Controller) Close() error {
	c.dns.close()
	c.tcp.close()
	return c.http.close()
}
