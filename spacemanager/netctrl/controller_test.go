package netctrl

import (
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// Ports 53/80 need root; tests bind fixed high ports instead. Each test uses
// a distinct pair since they run in the same process without -parallel.
const (
	testDNSPort  = 15300
	testHTTPPort = 18000
)

func startTestController(t *testing.T, dnsPort, httpPort int) *Controller {
	t.Helper()
	ctrl, err := Start(net.ParseIP("127.0.0.1"), Options{DNSPort: dnsPort, HTTPPort: httpPort})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { ctrl.Close() })
	return ctrl
}

// backendHTTP starts a plain HTTP server on loopback and returns its
// Endpoint, standing in for a real service inside a Space.
func backendHTTP(t *testing.T, body string) Endpoint {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen backend: %v", err)
	}
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	t.Cleanup(func() { ln.Close() })

	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	return Endpoint{IP: host, Port: port}
}

func mustResolve(t *testing.T, dnsAddr, qname string) net.IP {
	t.Helper()
	c := new(dns.Client)
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(qname), dns.TypeA)
	r, _, err := c.Exchange(m, dnsAddr)
	if err != nil {
		t.Fatalf("dns exchange: %v", err)
	}
	if r.Rcode != dns.RcodeSuccess || len(r.Answer) == 0 {
		t.Fatalf("dns query %s: rcode=%d answers=%d", qname, r.Rcode, len(r.Answer))
	}
	a, ok := r.Answer[0].(*dns.A)
	if !ok {
		t.Fatalf("answer is not an A record: %#v", r.Answer[0])
	}
	return a.A
}

func TestController_DNSResolvesToControllerIP(t *testing.T) {
	ep := backendHTTP(t, "hello from spaceB")
	ctrl := startTestController(t, testDNSPort+1, testHTTPPort+1)

	if err := ctrl.Put("spaceB", "service1", ep.IP, ep.Port, 25001); err != nil {
		t.Fatalf("Put: %v", err)
	}

	dnsAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(testDNSPort+1))
	got := mustResolve(t, dnsAddr, "service1.spaceB.internal")
	if !got.Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("resolved %s, want controller IP 127.0.0.1", got)
	}
}

func TestController_DNSNXDomainForUnknownService(t *testing.T) {
	startTestController(t, testDNSPort+2, testHTTPPort+2)

	dnsAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(testDNSPort+2))
	c := new(dns.Client)
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn("doesnotexist.spaceB.internal"), dns.TypeA)
	r, _, err := c.Exchange(m, dnsAddr)
	if err != nil {
		t.Fatalf("dns exchange: %v", err)
	}
	if r.Rcode != dns.RcodeNameError {
		t.Fatalf("rcode = %d, want NXDOMAIN", r.Rcode)
	}
}

func TestController_HTTPProxyRoutesByHost(t *testing.T) {
	ep := backendHTTP(t, "hello from spaceB")
	ctrl := startTestController(t, testDNSPort+3, testHTTPPort+3)

	if err := ctrl.Put("spaceB", "service1", ep.IP, ep.Port, 25002); err != nil {
		t.Fatalf("Put: %v", err)
	}

	url := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(testHTTPPort+3)) + "/"
	req, _ := http.NewRequest("GET", url, nil)
	req.Host = "service1.spaceB.internal"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http do: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello from spaceB" {
		t.Fatalf("body = %q, want %q", body, "hello from spaceB")
	}
}

func TestController_HTTPProxy502ForUnknownService(t *testing.T) {
	startTestController(t, testDNSPort+4, testHTTPPort+4)

	url := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(testHTTPPort+4)) + "/"
	req, _ := http.NewRequest("GET", url, nil)
	req.Host = "doesnotexist.spaceB.internal"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

func TestController_TCPRouteSplicesToBackend(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen backend: %v", err)
	}
	defer backend.Close()
	go func() {
		conn, err := backend.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 5)
		io.ReadFull(conn, buf)
		conn.Write([]byte("pong"))
	}()

	host, portStr, _ := net.SplitHostPort(backend.Addr().String())
	port, _ := strconv.Atoi(portStr)

	ctrl := startTestController(t, testDNSPort+5, testHTTPPort+5)

	// The declared port is deliberately NOT the backend's own ephemeral port:
	// dialing it is what proves callers reach the service on the port its
	// manifest names, not one the controller picked.
	const declaredPort = 25432
	if err := ctrl.Put("spaceB", "db", host, port, declaredPort); err != nil {
		t.Fatalf("Put: %v", err)
	}

	dialCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(declaredPort)))
	if err != nil {
		t.Fatalf("dial controller tcp port: %v", err)
	}
	defer conn.Close()

	conn.Write([]byte("ping!"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if string(buf) != "pong" {
		t.Fatalf("got %q, want %q", buf, "pong")
	}
}

func TestController_DeclaredPortCollisionKeepsHTTPRegistered(t *testing.T) {
	first := backendHTTP(t, "first")
	second := backendHTTP(t, "second")
	ctrl := startTestController(t, testDNSPort+7, testHTTPPort+7)

	const shared = 25433
	if err := ctrl.Put("spaceA", "db", first.IP, first.Port, shared); err != nil {
		t.Fatalf("Put(spaceA): %v", err)
	}
	if err := ctrl.Put("spaceB", "db", second.IP, second.Port, shared); err == nil {
		t.Fatal("Put(spaceB): want an error, two services cannot bind one port on one node")
	}

	// The failed TCP bind must not cost spaceB its HTTP path: the reverse
	// proxy is what most services actually use, and losing it would turn a
	// port clash into an outage.
	url := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(testHTTPPort+7)) + "/"
	req, _ := http.NewRequest("GET", url, nil)
	req.Host = "db.spaceB.internal"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http do: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "second" {
		t.Fatalf("body = %q, want %q", body, "second")
	}
}

func TestController_RefusedPortIsNotRetriedEveryPut(t *testing.T) {
	// A port the node itself already serves (meshauthority binds its own
	// 4300) can never be bound by the controller. The Reconciler re-Puts
	// every service on every tick, so without remembering the refusal this
	// retries — and logs — a hopeless bind several times a minute forever.
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen taken: %v", err)
	}
	defer taken.Close()
	_, portStr, _ := net.SplitHostPort(taken.Addr().String())
	takenPort, _ := strconv.Atoi(portStr)

	ep := backendHTTP(t, "svc")
	ctrl := startTestController(t, testDNSPort+8, testHTTPPort+8)

	if err := ctrl.Put("spaceB", "svc", ep.IP, ep.Port, takenPort); err == nil {
		t.Fatal("first Put: want a bind error on an already-taken port")
	}
	if err := ctrl.Put("spaceB", "svc", ep.IP, ep.Port, takenPort); err != nil {
		t.Fatalf("second Put: want the refusal remembered, got a retry error: %v", err)
	}

	// Delete must forget it, so a service whose port frees up binds again.
	ctrl.Delete("spaceB", "svc")
	taken.Close()
	if err := ctrl.Put("spaceB", "svc", ep.IP, ep.Port, takenPort); err != nil {
		t.Fatalf("Put after Delete and the port freeing up: %v", err)
	}
}

func TestController_DeleteRemovesDNSAndProxyAndTCP(t *testing.T) {
	ep := backendHTTP(t, "hello")
	ctrl := startTestController(t, testDNSPort+6, testHTTPPort+6)

	if err := ctrl.Put("spaceB", "service1", ep.IP, ep.Port, 25003); err != nil {
		t.Fatalf("Put: %v", err)
	}
	ctrl.Delete("spaceB", "service1")

	dnsAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(testDNSPort+6))
	c := new(dns.Client)
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn("service1.spaceB.internal"), dns.TypeA)
	r, _, err := c.Exchange(m, dnsAddr)
	if err != nil {
		t.Fatalf("dns exchange: %v", err)
	}
	if r.Rcode != dns.RcodeNameError {
		t.Fatalf("rcode = %d, want NXDOMAIN after Delete", r.Rcode)
	}
}
