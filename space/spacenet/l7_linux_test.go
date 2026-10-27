//go:build linux

package spacenet

import (
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/ingress"
)

func TestApplyL7Routes_DecodesAndSwaps(t *testing.T) {
	h := ingress.New()
	l7 := &l7Ingress{handler: h}
	js := `[{"host":"nginx.test","backend":"nginx:80"},{"host":"app.test","path":"/api","backend":"api:8080"}]`
	if err := applyL7Routes(l7, js); err != nil {
		t.Fatalf("applyL7Routes: %v", err)
	}
	// The handler's String() lists the routes it now serves.
	got := h.String()
	if got == "" {
		t.Fatal("expected routes after apply, got none")
	}
}

func TestApplyL7Routes_NilHandler_NoOp(t *testing.T) {
	if err := applyL7Routes(nil, `[{"host":"x","backend":"y:80"}]`); err != nil {
		t.Errorf("nil l7 should be a no-op, got %v", err)
	}
	if err := applyL7Routes(&l7Ingress{}, `[{"host":"x","backend":"y:80"}]`); err != nil {
		t.Errorf("nil handler should be a no-op, got %v", err)
	}
}

func TestApplyL7Routes_BadJSON_Errors(t *testing.T) {
	l7 := &l7Ingress{handler: ingress.New()}
	if err := applyL7Routes(l7, `{not json`); err == nil {
		t.Error("expected an error on malformed routes JSON")
	}
}

func TestApplyL7Routes_Empty_ClearsRoutes(t *testing.T) {
	h := ingress.New()
	l7 := &l7Ingress{handler: h}
	_ = applyL7Routes(l7, `[{"host":"x.test","backend":"x:80"}]`)
	if err := applyL7Routes(l7, ""); err != nil {
		t.Fatal(err)
	}
	if h.String() != "" {
		t.Errorf("empty routes should clear the table, got %q", h.String())
	}
}

func TestApplyL7Routes_ACMEManagesOnlyPublicHosts(t *testing.T) {
	acme, err := ingress.NewACMEResolver(ingress.ACMEConfig{
		Email:       "ops@example.com",
		StorageDir:  t.TempDir(),
		DNSProvider: "ovh",
		DNSCredentials: map[string]string{
			"OVH_APPLICATION_KEY":    "ak",
			"OVH_APPLICATION_SECRET": "as",
			"OVH_CONSUMER_KEY":       "ck",
		},
	})
	if err != nil {
		t.Fatalf("build acme resolver: %v", err)
	}
	defer acme.Stop()
	acme.DisableIssuanceForTest() // reconcile the managed set without a CA round-trip
	l7 := &l7Ingress{handler: ingress.New(), acme: acme}
	// A dev host and a public host: only the public one should be managed. The
	// call must not error even though the (fake) DNS creds can never solve — the
	// managed set is reconciled before any CA round-trip.
	js := `[{"host":"app.test","backend":"web:80"},{"host":"app.example.com","backend":"web:80"}]`
	if err := applyL7Routes(l7, js); err != nil {
		t.Fatalf("applyL7Routes: %v", err)
	}
	if acme.IsManagedForTest("app.test") {
		t.Error("dev host app.test must NOT be ACME-managed")
	}
	if !acme.IsManagedForTest("app.example.com") {
		t.Error("public host app.example.com must be ACME-managed")
	}
}

func TestGatewayIPFromCIDR(t *testing.T) {
	if ip := gatewayIPFromCIDR("10.88.0.1/24"); !ip.Equal(net.ParseIP("10.88.0.1")) {
		t.Errorf("got %v, want 10.88.0.1", ip)
	}
	if ip := gatewayIPFromCIDR("garbage"); ip != nil {
		t.Errorf("bad CIDR should yield nil, got %v", ip)
	}
}

// The L7 must drop a client that opens a connection and never finishes its
// request headers. The mux's peekTimeout does NOT cover this: it only bounds
// the peek, and the rate limiter runs in ServeHTTP, i.e. after the headers are
// fully read — so a slowloris reaches neither.
func TestNewL7Server_DropsClientThatNeverSendsHeaders(t *testing.T) {
	srv := newL7Server(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	if srv.ReadHeaderTimeout <= 0 {
		t.Fatal("newL7Server must set ReadHeaderTimeout — without it a slowloris holds the connection forever")
	}
	srv.ReadHeaderTimeout = 100 * time.Millisecond

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Enough to look like a request starting, never the blank line that ends
	// the headers — exactly what a slowloris sends.
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: app.example.com\r\n")); err != nil {
		t.Fatalf("write partial request: %v", err)
	}

	// The server must close on us rather than wait forever.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 64)
	if _, err = conn.Read(buf); err == nil {
		return // server answered (408) then closed: also a drop
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("connection still open after ReadHeaderTimeout — the slowloris was never dropped")
	}
}

// IdleTimeout and MaxHeaderBytes must be set too: the first bounds an
// established-then-silent keep-alive connection, the second caps how much
// memory an unauthenticated client can make the server hold per connection
// (Go's own default is 1 MiB).
func TestNewL7Server_BoundsIdleAndHeaderSize(t *testing.T) {
	srv := newL7Server(nil)
	if srv.IdleTimeout <= 0 {
		t.Error("newL7Server must set IdleTimeout")
	}
	if srv.MaxHeaderBytes <= 0 || srv.MaxHeaderBytes >= 1<<20 {
		t.Errorf("MaxHeaderBytes = %d; want a cap below Go's 1 MiB default", srv.MaxHeaderBytes)
	}
	// ReadTimeout/WriteTimeout are deliberately left unset: they are absolute
	// caps that would also cut a large upload, a slow download and every
	// WebSocket/SSE stream the reverse proxy passes through.
	if srv.ReadTimeout != 0 || srv.WriteTimeout != 0 {
		t.Errorf("ReadTimeout=%v WriteTimeout=%v; both must stay unset so long-lived streams survive", srv.ReadTimeout, srv.WriteTimeout)
	}
}
