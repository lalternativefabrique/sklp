//go:build linux

package spacenet

import (
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// TestNameTable_RoundRobin checks that lookup rotates the answer set across
// replicas and that unregister removes IPs (and the name once empty).
func TestNameTable_RoundRobin(t *testing.T) {
	tbl := newNameTable()
	a, b, c := net.ParseIP("10.0.0.2"), net.ParseIP("10.0.0.3"), net.ParseIP("10.0.0.4")
	tbl.register("DB", a) // mixed case: must match a lowercase lookup
	tbl.register("db", b)
	tbl.register("db", c)
	tbl.register("db", b) // dup: must not double-register

	first := []string{}
	for i := 0; i < 3; i++ {
		ips, ok := tbl.lookup("db")
		if !ok || len(ips) != 3 {
			t.Fatalf("lookup #%d = %v ok=%v, want 3 ips", i, ips, ok)
		}
		first = append(first, ips[0].String())
	}
	// Three consecutive lookups must rotate the head across all three IPs.
	if first[0] == first[1] || first[1] == first[2] || first[0] == first[2] {
		t.Fatalf("round-robin head did not rotate: %v", first)
	}

	tbl.unregister("db", b)
	if ips, _ := tbl.lookup("db"); len(ips) != 2 {
		t.Fatalf("after unregister: %v, want 2 ips", ips)
	}
	tbl.unregister("db", a)
	tbl.unregister("db", c)
	if _, ok := tbl.lookup("db"); ok {
		t.Fatalf("name should be gone after all IPs unregistered")
	}
}

// TestSpaceDNS_ServeAndForward exercises the resolver end to end in the
// current netns (no namespaces needed): a known service name is answered from
// the table, an unknown name is forwarded to a fake upstream. It binds on
// loopback rather than a bridge gateway so it runs anywhere.
func TestSpaceDNS_ServeAndForward(t *testing.T) {
	// Fake upstream: answers any A query with a fixed sentinel IP.
	const sentinel = "203.0.113.7"
	upPC, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("upstream listen: %v", err)
	}
	upMux := dns.NewServeMux()
	upMux.HandleFunc(".", func(w dns.ResponseWriter, req *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(req)
		m.Answer = append(m.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: req.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 5},
			A:   net.ParseIP(sentinel).To4(),
		})
		_ = w.WriteMsg(m)
	})
	upSrv := &dns.Server{PacketConn: upPC, Handler: upMux}
	go func() { _ = upSrv.ActivateAndServe() }()
	defer upSrv.Shutdown()
	upstreamAddr := upPC.LocalAddr().String()

	// Resolver under test, on loopback, forwarding to the fake upstream.
	names := newNameTable()
	names.register("db", net.ParseIP("10.88.0.3"))
	d, err := newSpaceDNS(names, net.ParseIP("127.0.0.1"), "0", []string{upstreamAddr}, "")
	if err != nil {
		t.Fatalf("newSpaceDNS: %v", err)
	}
	defer d.close()

	udpAddr := d.udp.PacketConn.LocalAddr().String()
	cl := &dns.Client{Timeout: 2 * time.Second}

	// Known service name -> table IP.
	got := queryA(t, cl, udpAddr, "db")
	if len(got) != 1 || got[0] != "10.88.0.3" {
		t.Fatalf("resolve db = %v, want [10.88.0.3]", got)
	}

	// Unknown name -> forwarded to upstream -> sentinel.
	got = queryA(t, cl, udpAddr, "registry.npmjs.org")
	if len(got) != 1 || got[0] != sentinel {
		t.Fatalf("forward unknown = %v, want [%s]", got, sentinel)
	}
}

// TestSpaceDNS_AAAAIsNoData pins the v4-only guard: an AAAA query — for a
// service name OR an external name — must get NODATA (NOERROR, empty answer),
// never a forwarded IPv6. Space egress is slirp4netns (IPv4-only), so any AAAA
// answer would hand the client an unroutable IPv6 it prefers over the A record
// (the S3/HTTPS "Forbidden"-from-inside-a-space failure). The fake upstream
// here DOES answer AAAA, so a forwarded AAAA would leak through if the guard
// regressed.
func TestSpaceDNS_AAAAIsNoData(t *testing.T) {
	upPC, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("upstream listen: %v", err)
	}
	upMux := dns.NewServeMux()
	upMux.HandleFunc(".", func(w dns.ResponseWriter, req *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(req)
		m.Answer = append(m.Answer, &dns.AAAA{
			Hdr:  dns.RR_Header{Name: req.Question[0].Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 5},
			AAAA: net.ParseIP("2001:db8::1"),
		})
		_ = w.WriteMsg(m)
	})
	upSrv := &dns.Server{PacketConn: upPC, Handler: upMux}
	go func() { _ = upSrv.ActivateAndServe() }()
	defer upSrv.Shutdown()

	names := newNameTable()
	names.register("db", net.ParseIP("10.88.0.3"))
	d, err := newSpaceDNS(names, net.ParseIP("127.0.0.1"), "0", []string{upPC.LocalAddr().String()}, "")
	if err != nil {
		t.Fatalf("newSpaceDNS: %v", err)
	}
	defer d.close()

	udpAddr := d.udp.PacketConn.LocalAddr().String()
	cl := &dns.Client{Timeout: 2 * time.Second}

	// Both a known service name and an external name must be NODATA on AAAA.
	for _, name := range []string{"db", "s3.fr-par.scw.cloud"} {
		m := new(dns.Msg)
		m.SetQuestion(dns.Fqdn(name), dns.TypeAAAA)
		resp, _, err := cl.Exchange(m, udpAddr)
		if err != nil {
			t.Fatalf("AAAA exchange %q: %v", name, err)
		}
		if resp.Rcode != dns.RcodeSuccess {
			t.Errorf("AAAA %q rcode = %s, want NOERROR", name, dns.RcodeToString[resp.Rcode])
		}
		if len(resp.Answer) != 0 {
			t.Errorf("AAAA %q returned %d answer(s), want NODATA (0)", name, len(resp.Answer))
		}
	}
}

func queryA(t *testing.T, cl *dns.Client, server, name string) []string {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	resp, _, err := cl.Exchange(m, server)
	if err != nil {
		t.Fatalf("exchange %q: %v", name, err)
	}
	var out []string
	for _, rr := range resp.Answer {
		if a, ok := rr.(*dns.A); ok {
			out = append(out, net.IP(a.A).String())
		}
	}
	return out
}

func TestReachableFromSpace(t *testing.T) {
	cases := map[string]bool{
		"1.1.1.1":       true,  // public
		"8.8.8.8":       true,  // public
		"192.168.1.1":   false, // RFC1918 home router
		"10.0.0.1":      false, // RFC1918
		"172.16.0.1":    false, // RFC1918
		"127.0.0.53":    false, // loopback (systemd-resolved)
		"169.254.0.1":   false, // link-local
		"0.250.250.200": false, // 0.0.0.0/8 host-side magic (OrbStack's resolver)
	}
	for s, want := range cases {
		if got := reachableFromSpace(net.ParseIP(s)); got != want {
			t.Errorf("reachableFromSpace(%s) = %v, want %v", s, got, want)
		}
	}
}

func TestReachableFromSpace_IPv6Unroutable(t *testing.T) {
	// A PUBLIC IPv6 resolver is still unreachable (slirp is IPv4-only).
	if reachableFromSpace(net.ParseIP("2a01:cb18:81f6:3300::1")) {
		t.Error("public IPv6 must be unreachable from the space (slirp v4-only)")
	}
}

// TestSpaceDNS_ControllerNeverSwallowsPublicNames pins forward's partition
// around the Network Controller's resolver: the controller answers NXDOMAIN
// for every non-.internal name (it is only the internal zone's authority),
// and being first in the upstream list that valid response used to end the
// loop — no public name resolved from inside a deployed space (S3 endpoints,
// package mirrors). A .internal name must go to the controller ONLY, so its
// NXDOMAIN stays final and internal names never leak to a public resolver.
func TestSpaceDNS_ControllerNeverSwallowsPublicNames(t *testing.T) {
	const meshIP = "10.90.0.2"
	const sentinel = "203.0.113.9"

	serve := func(handler dns.HandlerFunc) (addr string, shutdown func()) {
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("upstream listen: %v", err)
		}
		mux := dns.NewServeMux()
		mux.HandleFunc(".", handler)
		srv := &dns.Server{PacketConn: pc, Handler: mux}
		go func() { _ = srv.ActivateAndServe() }()
		return pc.LocalAddr().String(), func() { _ = srv.Shutdown() }
	}

	// Fake controller: authoritative for the internal zone — answers its one
	// known name, NXDOMAIN for everything else (public names included).
	ctrlAddr, stopCtrl := serve(func(w dns.ResponseWriter, req *dns.Msg) {
		m := new(dns.Msg)
		if req.Question[0].Name == "web.alpha.internal." {
			m.SetReply(req)
			m.Answer = append(m.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: req.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 5},
				A:   net.ParseIP(meshIP).To4(),
			})
		} else {
			m.SetRcode(req, dns.RcodeNameError)
		}
		_ = w.WriteMsg(m)
	})
	defer stopCtrl()

	// Fake public resolver: answers anything — but must NEVER see .internal.
	var publicSawInternal atomic.Bool
	pubAddr, stopPub := serve(func(w dns.ResponseWriter, req *dns.Msg) {
		if strings.HasSuffix(req.Question[0].Name, ".internal.") {
			publicSawInternal.Store(true)
		}
		m := new(dns.Msg)
		m.SetReply(req)
		m.Answer = append(m.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: req.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 5},
			A:   net.ParseIP(sentinel).To4(),
		})
		_ = w.WriteMsg(m)
	})
	defer stopPub()

	// ctrlAddr stands in for envControllerIP:53, unreachable from a test.
	d, err := newSpaceDNS(newNameTable(), net.ParseIP("127.0.0.1"), "0", []string{ctrlAddr, pubAddr}, ctrlAddr)
	if err != nil {
		t.Fatalf("newSpaceDNS: %v", err)
	}
	defer d.close()

	udpAddr := d.udp.PacketConn.LocalAddr().String()
	cl := &dns.Client{Timeout: 2 * time.Second}

	// A public name resolves via the public upstream, controller skipped.
	if got := queryA(t, cl, udpAddr, "s3.gra.io.cloud.ovh.net"); len(got) != 1 || got[0] != sentinel {
		t.Fatalf("public name = %v, want [%s] — the controller's NXDOMAIN swallowed it", got, sentinel)
	}

	// An .internal name resolves via the controller only.
	if got := queryA(t, cl, udpAddr, "web.alpha.internal"); len(got) != 1 || got[0] != meshIP {
		t.Fatalf("internal name = %v, want [%s]", got, meshIP)
	}

	// An unknown .internal name is NXDOMAIN — final, never retried publicly.
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn("ghost.nowhere.internal"), dns.TypeA)
	r, _, err := cl.Exchange(m, udpAddr)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if r.Rcode != dns.RcodeNameError {
		t.Fatalf("unknown internal rcode = %d, want NXDOMAIN", r.Rcode)
	}
	if publicSawInternal.Load() {
		t.Fatal("a .internal query leaked to the public resolver")
	}
}
