package netctrl

import (
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/spacemanager/mesh"
	"github.com/miekg/dns"
)

func TestPeerCache_RemoteEndpoint(t *testing.T) {
	pc := NewPeerCache("self-id")
	pc.Refresh([]mesh.Peer{
		{ID: "self-id", PrivateIP: "10.90.0.1", Services: []mesh.ServiceRef{{Space: "spaceA", Service: "should-be-skipped", Port: 80}}},
		{ID: "peer-2", PrivateIP: "10.90.0.2", Services: []mesh.ServiceRef{{Space: "spaceB", Service: "api", Port: 8080}}},
	})

	if ip, ok := pc.RemoteEndpoint("spaceB", "api"); !ok || ip != "10.90.0.2" {
		t.Fatalf("RemoteEndpoint(spaceB, api) = (%q, %v), want (10.90.0.2, true)", ip, ok)
	}
	if _, ok := pc.RemoteEndpoint("spaceA", "should-be-skipped"); ok {
		t.Fatal("self peer's own service must not appear in the remote cache")
	}
	if _, ok := pc.RemoteEndpoint("spaceZ", "ghost"); ok {
		t.Fatal("unknown service must miss")
	}
}

func TestPeerCache_StalePeerEvicted(t *testing.T) {
	stale := time.Now().Add(-peerStaleAfter - time.Second)
	fresh := time.Now()
	pc := NewPeerCache("self-id")
	pc.Refresh([]mesh.Peer{
		{ID: "dead", PrivateIP: "10.90.0.4", LastSeenAt: &stale, Services: []mesh.ServiceRef{{Space: "spaceD", Service: "gone", Port: 80}}},
		{ID: "alive", PrivateIP: "10.90.0.5", LastSeenAt: &fresh, Services: []mesh.ServiceRef{{Space: "spaceE", Service: "up", Port: 80}}},
		{ID: "no-timestamp", PrivateIP: "10.90.0.6", Services: []mesh.ServiceRef{{Space: "spaceF", Service: "kept", Port: 80}}},
	})

	if _, ok := pc.RemoteEndpoint("spaceD", "gone"); ok {
		t.Fatal("a peer not seen within peerStaleAfter must stop resolving")
	}
	if _, ok := pc.RemoteEndpoint("spaceE", "up"); !ok {
		t.Fatal("a fresh peer must keep resolving")
	}
	if _, ok := pc.RemoteEndpoint("spaceF", "kept"); !ok {
		t.Fatal("a peer with no LastSeenAt must be treated as fresh, never evicted on absent data")
	}
}

// A second peer publishing a space it does not run used to win whenever its
// ID sorted last, taking every request the front routes there.
func TestPeerCache_AContestedServiceResolvesToNothing(t *testing.T) {
	stale := time.Now().Add(-peerStaleAfter - time.Second)
	web := mesh.ServiceRef{Space: "shop-production", Service: "web", Port: 80}
	pc := NewPeerCache("self-id")
	pc.Refresh([]mesh.Peer{
		{ID: "node-a", PrivateIP: "10.90.0.2", Services: []mesh.ServiceRef{web, {Space: "acme-production", Service: "web", Port: 80}}},
		{ID: "zzz-intruder", PrivateIP: "10.90.0.9", Services: []mesh.ServiceRef{web}},
		{ID: "gone", PrivateIP: "10.90.0.7", LastSeenAt: &stale, Services: []mesh.ServiceRef{{Space: "acme-production", Service: "web", Port: 80}}},
	})

	if ip, ok := pc.RemoteEndpoint("shop-production", "web"); ok {
		t.Fatalf("contested service resolved to %s", ip)
	}
	if ip, ok := pc.RemoteEndpoint("acme-production", "web"); !ok || ip != "10.90.0.2" {
		t.Fatalf("a stale peer's claim blocked a live one: (%q, %v)", ip, ok)
	}
}

func TestPeerCache_RefreshReplacesWholesale(t *testing.T) {
	pc := NewPeerCache("self-id")
	pc.Refresh([]mesh.Peer{{ID: "peer-2", PrivateIP: "10.90.0.2", Services: []mesh.ServiceRef{{Space: "spaceB", Service: "api", Port: 8080}}}})
	if _, ok := pc.RemoteEndpoint("spaceB", "api"); !ok {
		t.Fatal("expected a hit after first refresh")
	}

	pc.Refresh([]mesh.Peer{{ID: "peer-2", PrivateIP: "10.90.0.2", Services: nil}})
	if _, ok := pc.RemoteEndpoint("spaceB", "api"); ok {
		t.Fatal("stale entry from a prior refresh must not survive a wholesale replace")
	}
}

func TestPeerSignature_IgnoresServiceChurn(t *testing.T) {
	seen1 := time.Now()
	seen2 := seen1.Add(10 * time.Second)
	base := []mesh.Peer{
		{ID: "a", PubKey: "ka", Endpoint: "1.2.3.4:51820", PrivateIP: "10.90.0.2", LastSeenAt: &seen1},
		{ID: "b", PubKey: "kb", Endpoint: "1.2.3.5:51820", PrivateIP: "10.90.0.3"},
	}
	churned := []mesh.Peer{
		{ID: "b", PubKey: "kb", Endpoint: "1.2.3.5:51820", PrivateIP: "10.90.0.3", Services: []mesh.ServiceRef{{Space: "s", Service: "new", Port: 80}}},
		{ID: "a", PubKey: "ka", Endpoint: "1.2.3.4:51820", PrivateIP: "10.90.0.2", LastSeenAt: &seen2},
	}
	if peerSignature(base) != peerSignature(churned) {
		t.Fatal("service list / last-seen churn and ordering must not change the tunnel signature")
	}

	rekeyed := []mesh.Peer{base[0], {ID: "b", PubKey: "kb2", Endpoint: "1.2.3.5:51820", PrivateIP: "10.90.0.3"}}
	if peerSignature(base) == peerSignature(rekeyed) {
		t.Fatal("a pubkey change must change the tunnel signature")
	}
}

func TestController_DNSLocalFirstThenRemote(t *testing.T) {
	ep := backendHTTP(t, "local")
	ctrl := startTestController(t, testDNSPort+10, testHTTPPort+10)

	// spaceA/local is registered in this controller's own Registry.
	if err := ctrl.Put("spaceA", "local", ep.IP, ep.Port, 25004); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// spaceB/remote only exists in the peer cache, as if reported by
	// another node over meshauthority.
	ctrl.Peers().Refresh([]mesh.Peer{
		{ID: "peer-2", PrivateIP: "10.90.0.9", Services: []mesh.ServiceRef{{Space: "spaceB", Service: "remote", Port: 8080}}},
	})

	dnsAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(testDNSPort+10))

	// Local hit answers the controller's own IP (selfIP), never the peer's.
	gotLocal := mustResolve(t, dnsAddr, "local.spaceA.internal")
	if !gotLocal.Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("local.spaceA.internal resolved to %s, want controller's own IP 127.0.0.1", gotLocal)
	}

	// Remote hit answers the PEER's mesh IP directly, not selfIP.
	gotRemote := mustResolve(t, dnsAddr, "remote.spaceB.internal")
	if !gotRemote.Equal(net.ParseIP("10.90.0.9")) {
		t.Fatalf("remote.spaceB.internal resolved to %s, want peer mesh IP 10.90.0.9", gotRemote)
	}

	// Neither local nor remote: NXDOMAIN.
	c := new(dns.Client)
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn("ghost.spaceC.internal"), dns.TypeA)
	r, _, err := c.Exchange(m, dnsAddr)
	if err != nil {
		t.Fatalf("dns exchange: %v", err)
	}
	if r.Rcode != dns.RcodeNameError {
		t.Fatalf("rcode = %d, want NXDOMAIN", r.Rcode)
	}
}

func TestPeerCache_NodeHost(t *testing.T) {
	pc := NewPeerCache("self-id")
	old := time.Now().Add(-2 * peerStaleAfter)
	pc.Refresh([]mesh.Peer{
		{ID: "self-id", PrivateIP: "10.90.0.1", Endpoint: "10.73.0.10:51820"},
		{ID: "origin-a", PrivateIP: "10.90.0.3", Endpoint: "10.73.0.20:51820"},
		{ID: "no-endpoint", PrivateIP: "10.90.0.4"},
		{ID: "stale", PrivateIP: "10.90.0.5", Endpoint: "10.73.0.30:51820", LastSeenAt: &old},
	})

	if host, ok := pc.NodeHost("origin-a"); !ok || host != "10.73.0.20" {
		t.Fatalf("NodeHost(origin-a) = (%q, %v), want (10.73.0.20, true)", host, ok)
	}
	for _, id := range []string{"self-id", "no-endpoint", "stale", "ghost"} {
		if _, ok := pc.NodeHost(id); ok {
			t.Fatalf("NodeHost(%s) must miss", id)
		}
	}
}
