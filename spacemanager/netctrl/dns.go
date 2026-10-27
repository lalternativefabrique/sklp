package netctrl

import (
	"fmt"
	"net"
	"strings"

	"github.com/miekg/dns"
)

// dnsTTL is deliberately short: a service can be deregistered at any time
// (Space teardown, healthcheck flip) and nothing should cache a dead answer.
const dnsTTL = 5

// dnsServer answers <service>.<space>.internal queries: a local Registry hit
// returns the controller's own IP (never the real endpoint — the HTTP proxy
// and per-service TCP listeners do the actual routing once traffic reaches
// that IP, and now also listen on the mesh interface for cross-node
// traffic — see controller.go). A local miss falls through to peers, the
// last-known mesh peer list (nil when mesh is disabled): a hit there
// answers with that REMOTE node's own mesh IP directly, so the caller
// dials it over the existing WireGuard tunnel with no relay hop through
// this controller. Both miss = NXDOMAIN, never forwarded, so a typo never
// leaks to a public resolver. Anything not shaped like
// <service>.<space>.internal is also NXDOMAIN: the controller is not a
// general-purpose resolver, only the internal zone's authority.
type dnsServer struct {
	reg    *Registry
	selfIP net.IP
	peers  *PeerCache
	udp    *dns.Server
	tcp    *dns.Server
}

// internalSuffix is the zone this controller is authoritative for.
const internalSuffix = ".internal."

// parseInternalName splits "service.space.internal." into (service, space).
// ok=false for anything not shaped exactly like a 3-label name under
// .internal — that is not this controller's problem to answer.
func parseInternalName(qname string) (service, space string, ok bool) {
	lower := strings.ToLower(qname)
	if !strings.HasSuffix(lower, internalSuffix) {
		return "", "", false
	}
	base := strings.TrimSuffix(lower, internalSuffix)
	labels := strings.Split(base, ".")
	if len(labels) != 2 || labels[0] == "" || labels[1] == "" {
		return "", "", false
	}
	return labels[0], labels[1], true
}

func newDNSServer(reg *Registry, selfIP net.IP, addr string, peers *PeerCache) (*dnsServer, error) {
	d := &dnsServer{reg: reg, selfIP: selfIP, peers: peers}

	mux := dns.NewServeMux()
	mux.HandleFunc(".", d.handle)

	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("netctrl: dns udp listen %s: %w", addr, err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("netctrl: dns tcp listen %s: %w", addr, err)
	}

	d.udp = &dns.Server{PacketConn: pc, Handler: mux}
	d.tcp = &dns.Server{Listener: ln, Handler: mux}
	go d.udp.ActivateAndServe()
	go d.tcp.ActivateAndServe()
	return d, nil
}

func (d *dnsServer) handle(w dns.ResponseWriter, req *dns.Msg) {
	if len(req.Question) != 1 {
		d.nxdomain(w, req)
		return
	}
	q := req.Question[0]
	if q.Qtype == dns.TypeAAAA {
		d.nodata(w, req)
		return
	}
	service, space, ok := parseInternalName(q.Name)
	if !ok {
		d.nxdomain(w, req)
		return
	}
	if _, ok := d.reg.Lookup(space, service); ok {
		d.answerA(w, req, q, d.selfIP)
		return
	}
	if d.peers != nil {
		if meshIP, ok := d.peers.RemoteEndpoint(space, service); ok {
			if ip := net.ParseIP(meshIP); ip != nil {
				d.answerA(w, req, q, ip)
				return
			}
		}
	}
	d.nxdomain(w, req)
}

func (d *dnsServer) answerA(w dns.ResponseWriter, req *dns.Msg, q dns.Question, ip net.IP) {
	msg := new(dns.Msg)
	msg.SetReply(req)
	msg.Authoritative = true
	msg.Answer = append(msg.Answer, &dns.A{
		Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: dnsTTL},
		A:   ip,
	})
	w.WriteMsg(msg)
}

func (d *dnsServer) nxdomain(w dns.ResponseWriter, req *dns.Msg) {
	msg := new(dns.Msg)
	msg.SetRcode(req, dns.RcodeNameError)
	w.WriteMsg(msg)
}

func (d *dnsServer) nodata(w dns.ResponseWriter, req *dns.Msg) {
	msg := new(dns.Msg)
	msg.SetReply(req)
	msg.Authoritative = true
	w.WriteMsg(msg)
}

func (d *dnsServer) close() {
	d.udp.Shutdown()
	d.tcp.Shutdown()
}
