//go:build linux

package spacenet

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/miekg/dns"
	"github.com/vishvananda/netns"
)

// dnsTTL is the A-record TTL handed to space clients. It is deliberately
// short: a replica can appear/disappear at any time, and space DNS is a
// loopback-cheap lookup, so we do not want resolvers caching a dead IP.
const dnsTTL = 5

// upstreamTimeout caps how long we wait on the host's resolver before failing
// a forwarded (non-service) query, so a flaky upstream never hangs a client.
const upstreamTimeout = 3 * time.Second

// spaceDNS is the in-space resolver. It answers A queries for registered
// service names from the name table and forwards everything else to the host's
// upstream resolvers. It runs inside the space netns, bound to the bridge
// gateway, so every attached container reaches it at <gateway>:53.
type spaceDNS struct {
	names    *nameTable
	gateway  net.IP
	upstream []string // host resolvers, "ip:53"
	// ctrl is the Network Controller's resolver ("ip:53") when one is
	// configured, empty otherwise. It marks which upstream entry forward's
	// .internal/public partition applies to (see forward's doc comment).
	ctrl string

	udp *dns.Server
	tcp *dns.Server
}

// dnsPort is where the space resolver listens on the gateway. Tests pass a
// "0" port to bind an ephemeral loopback socket without privileges.
const dnsPort = "53"

// newSpaceDNS binds UDP and TCP listeners on gateway:port in the CURRENT
// netns (the worker calls it after entering the space netns) and starts
// serving. The caller must have its OS thread pinned to the space netns; the
// bound sockets then stay in that netns regardless of later goroutine
// scheduling.
func newSpaceDNS(names *nameTable, gateway net.IP, port string, upstream []string, ctrl string) (*spaceDNS, error) {
	addr := net.JoinHostPort(gateway.String(), port)

	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("spacenet: dns udp listen %s: %w", addr, err)
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("spacenet: dns tcp listen %s: %w", addr, err)
	}

	d := &spaceDNS{names: names, gateway: gateway, upstream: upstream, ctrl: ctrl}
	mux := dns.NewServeMux()
	mux.HandleFunc(".", d.handle)
	d.udp = &dns.Server{PacketConn: pc, Handler: mux}
	d.tcp = &dns.Server{Listener: l, Handler: mux}

	go func() { _ = d.udp.ActivateAndServe() }()
	go func() { _ = d.tcp.ActivateAndServe() }()
	return d, nil
}

func (d *spaceDNS) close() {
	if d == nil {
		return
	}
	if d.udp != nil {
		_ = d.udp.Shutdown()
	}
	if d.tcp != nil {
		_ = d.tcp.Shutdown()
	}
}

// handle answers a single query: a service-name A/AAAA lookup is served from
// the name table; anything else is forwarded upstream. Names are matched
// without the trailing dot and case-insensitively (the table lowercases).
//
// AAAA is answered locally with NODATA for EVERY name, service or external:
// space egress is slirp4netns (IPv4-only), so a AAAA answer would hand the
// client an unroutable IPv6 it prefers over the working A record — the failure
// mode behind S3/HTTPS "Forbidden" from inside a space (getaddrinfo returns the
// AAAA, the client connects over v6, slirp drops it). Returning NODATA on AAAA
// forces the client back to the IPv4 A record.
func (d *spaceDNS) handle(w dns.ResponseWriter, req *dns.Msg) {
	if len(req.Question) == 1 {
		q := req.Question[0]
		if q.Qtype == dns.TypeAAAA {
			d.answerNoData(w, req)
			return
		}
		name := strings.TrimSuffix(q.Name, ".")
		if ips, ok := d.names.lookup(name); ok {
			d.answerService(w, req, q, ips)
			return
		}
	}
	d.forward(w, req)
}

// answerNoData replies NOERROR with an empty answer section — the correct
// response for a name that exists but has no record of the queried type. Used
// for AAAA in a v4-only space.
func (d *spaceDNS) answerNoData(w dns.ResponseWriter, req *dns.Msg) {
	resp := new(dns.Msg)
	resp.SetReply(req)
	resp.Authoritative = true
	_ = w.WriteMsg(resp)
}

// answerService replies for a known service name. A queries get the rotated
// IPv4 set; other types (AAAA is handled earlier) get an empty NOERROR.
func (d *spaceDNS) answerService(w dns.ResponseWriter, req *dns.Msg, q dns.Question, ips []net.IP) {
	resp := new(dns.Msg)
	resp.SetReply(req)
	resp.Authoritative = true
	if q.Qtype == dns.TypeA {
		for _, ip := range ips {
			v4 := ip.To4()
			if v4 == nil {
				continue
			}
			resp.Answer = append(resp.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: dnsTTL},
				A:   v4,
			})
		}
	}
	_ = w.WriteMsg(resp)
}

// forward relays a query to the host's upstream resolvers, returning the first
// answer. With no upstreams configured, or none reachable (e.g. the space
// has no egress yet — that route arrives with NAT in a later step), it returns
// SERVFAIL: service names still resolve locally; external names fail until
// egress exists. The dependency is intentional and isolated here.
//
// The upstream list is PARTITIONED around the Network Controller's resolver
// when one is configured: a .internal question goes to the controller ONLY
// (its NXDOMAIN is authoritative and final — an internal name must never
// leak to a public resolver), and every other question SKIPS the controller.
// Without the skip, the controller being first in the list swallowed all
// public resolution: it answers NXDOMAIN for every non-.internal name, which
// is a valid response the loop below accepts — inside a deployed space, S3
// endpoints and package mirrors stopped resolving entirely.
func (d *spaceDNS) forward(w dns.ResponseWriter, req *dns.Msg) {
	ctrl := d.ctrl
	internal := len(req.Question) == 1 &&
		strings.HasSuffix(strings.ToLower(req.Question[0].Name), ".internal.")

	c := &dns.Client{Timeout: upstreamTimeout}
	if _, tcp := w.RemoteAddr().(*net.TCPAddr); tcp {
		c.Net = "tcp"
	}
	for _, up := range d.upstream {
		if ctrl != "" && internal != (up == ctrl) {
			continue
		}
		if resp, _, err := c.Exchange(req, up); err == nil && resp != nil {
			_ = w.WriteMsg(resp)
			return
		}
	}
	fail := new(dns.Msg)
	fail.SetRcode(req, dns.RcodeServerFailure)
	_ = w.WriteMsg(fail)
}

// publicDNSFallback are public resolvers used as a LAST RESORT for forwarding
// external names. They have PUBLIC IPs, so unlike a host LAN resolver they are
// not caught by the space's RFC1918 egress blackhole (see egress.go) and stay
// reachable over slirp. Appended after the host's own reachable resolvers, so a
// public/usable host resolver still wins; they only matter when every host
// resolver is unreachable from the space.
var publicDNSFallback = []string{"1.1.1.1:53", "8.8.8.8:53"}

// hostUpstreams reads the host's /etc/resolv.conf nameservers (the worker is in
// the space's net+user namespace but the host mount namespace, so this is the
// host file) and returns them as "ip:53", with a public-resolver fallback.
//
// The catch: the space blackholes RFC1918 + loopback for SSRF defence, so a
// host resolver on the LAN (192.168.x.x — a home router) or on loopback
// (127.0.0.53 — systemd-resolved) is UNREACHABLE from inside the space. A
// service that resolves an external name (e.g. `go run` fetching modules) would
// then fail. We therefore append public resolvers whenever the host's own
// resolvers are all unreachable-from-space, so external resolution still
// works while the blackhole (and thus the SSRF protection) stays intact.
func hostUpstreams() (upstreams []string, ctrl string) {
	// The Network Controller (spacemanager/netctrl), when running, is the
	// authority for <service>.<space>.internal names — tried FIRST, ahead of
	// the host's own resolvers, so a cross-space lookup never depends on
	// whatever the host happens to resolve. It is deliberately exempted from
	// reachableFromSpace's RFC1918 filter: unlike an arbitrary LAN resolver,
	// the controller's /32 is punched through the egress blackhole on
	// purpose (allowcontroller_linux.go) specifically so this lookup works.
	if ctrlIP := os.Getenv(envControllerIP); ctrlIP != "" {
		ctrl = net.JoinHostPort(ctrlIP, "53")
		upstreams = append(upstreams, ctrl)
	}

	f, err := os.Open("/etc/resolv.conf")
	if err != nil {
		return append(upstreams, publicDNSFallback...), ctrl
	}
	defer f.Close()

	// Keep only host resolvers actually reachable from the space, in file
	// order. An unreachable one (LAN/loopback/IPv6) is dropped, not kept first —
	// otherwise forward() wastes a full upstreamTimeout on it before every
	// external lookup.
	var reachable []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "nameserver" {
			if ip := net.ParseIP(fields[1]); ip != nil && reachableFromSpace(ip) {
				reachable = append(reachable, net.JoinHostPort(fields[1], "53"))
			}
		}
	}
	// No reachable host resolver (the common dev case: a LAN/loopback resolver)
	// → fall back to public ones so external names still resolve. The blackhole
	// (and thus SSRF protection) is untouched.
	if len(reachable) == 0 {
		return append(upstreams, publicDNSFallback...), ctrl
	}
	return append(upstreams, reachable...), ctrl
}

// reachableFromSpace reports whether a resolver IP is reachable from inside
// the space netns. Two gates: it must be a PUBLIC address (a private /
// loopback / link-local one is caught by the SSRF egress blackhole), AND it
// must be IPv4 — space egress is slirp4netns, which is IPv4-only, so even a
// public IPv6 resolver (common in a dual-stack /etc/resolv.conf) is unroutable.
func reachableFromSpace(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false // IPv6 — no route out of the space (slirp is v4-only)
	}
	if v4[0] == 0 {
		// 0.0.0.0/8 ("this network") is never a routable unicast destination;
		// a resolver there is host-side magic (OrbStack's 0.250.250.200 is
		// answered only in the machine's root netns), dead from the space.
		return false
	}
	return !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast()
}

// enterSpaceNetns pins the calling goroutine's OS thread and moves it into
// capNS, used by the DNS goroutine before it binds so its sockets live in the
// space netns. It deliberately does NOT unlock the thread: the listeners must
// stay bound in that netns for the worker's lifetime.
func enterSpaceNetns(capNS netns.NsHandle) error {
	runtime.LockOSThread()
	return netns.Set(capNS)
}
