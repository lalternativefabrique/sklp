package space

import (
	"bufio"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRouter records the splice it is asked to perform and echoes the replayed
// prefix bytes back to the client, so a test asserts both the routing decision
// (service/port) and that the peeked bytes are not lost.
type fakeRouter struct {
	mu      sync.Mutex
	service string
	port    int
	prefix  []byte
	err     error
}

func (f *fakeRouter) SendConnReplay(service string, port int, client net.Conn, prefix []byte) error {
	f.mu.Lock()
	f.service = service
	f.port = port
	f.prefix = append([]byte(nil), prefix...)
	f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	// Echo the replayed request bytes back so the client can verify nothing was
	// dropped during the Host-header peek, then close.
	go func() {
		_, _ = client.Write(prefix)
		_ = client.Close()
	}()
	return nil
}

// TestHostMux_PeekHostHeader covers the request-line/Host parsing + the replay
// buffer the splice needs.
func TestHostMux_PeekHostHeader(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantHost string
		wantErr  bool
	}{
		{"simple", "GET / HTTP/1.1\r\nHost: core.abc.localhost\r\n\r\n", "core.abc.localhost", false},
		{"host with port", "GET /x HTTP/1.1\r\nHost: web.abc.localhost:80\r\n\r\n", "web.abc.localhost:80", false},
		{"extra headers", "POST /a HTTP/1.1\r\nUser-Agent: t\r\nHost: api.localhost\r\n\r\n", "api.localhost", false},
		{"no host", "GET / HTTP/1.0\r\n\r\n", "", true},
		{"garbage", "not http at all\r\n\r\n", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			br := bufio.NewReader(strings.NewReader(tc.raw))
			host, buffered, err := peekHostHeader(br)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got host=%q", host)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if host != tc.wantHost {
				t.Errorf("host = %q, want %q", host, tc.wantHost)
			}
			if string(buffered) != tc.raw {
				t.Errorf("replay buffer = %q, want full request %q", buffered, tc.raw)
			}
		})
	}
}

// TestHostMux_RouteByHost drives an end-to-end accept: a registered host routes
// to its router with the replayed bytes; an unknown host is closed.
func TestHostMux_RouteByHost(t *testing.T) {
	m := newHostMux(0) // 0 = ephemeral loopback port (no collision with a real dev mux)
	if err := m.start(); err != nil {
		t.Fatalf("bind ephemeral: %v", err)
	}
	defer m.close()

	r := &fakeRouter{}
	m.register("core.abc.localhost", route{router: r, service: "core", port: 4100})

	// Dial the live listener and send a request for the registered host.
	conn, err := net.Dial("tcp", m.ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	req := "GET /health HTTP/1.1\r\nHost: core.abc.localhost\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write: %v", err)
	}
	echo := make([]byte, len(req))
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := readFull(conn, echo); err != nil {
		t.Fatalf("read echo: %v", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.service != "core" || r.port != 4100 {
		t.Errorf("routed to %s:%d, want core:4100", r.service, r.port)
	}
	if string(r.prefix) != req {
		t.Errorf("replayed prefix = %q, want full request %q", r.prefix, req)
	}
}

func readFull(c net.Conn, p []byte) (int, error) {
	got := 0
	for got < len(p) {
		n, err := c.Read(p[got:])
		got += n
		if err != nil {
			return got, err
		}
	}
	return got, nil
}

// TestHostMuxManager_RefCount proves the singleton opens on first acquire and
// closes only on the last release, mirroring controller.gatewayManager.
func TestHostMuxManager_RefCount(t *testing.T) {
	// Acquire on an ephemeral loopback port (0) to avoid colliding with a real
	// dev mux, and reset the package singleton so a prior test does not bleed in.
	hostMuxSingleton.mu.Lock()
	hostMuxSingleton.mux = nil
	hostMuxSingleton.refs = 0
	hostMuxSingleton.mu.Unlock()

	h1, err := AcquireHostMux(0)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	h2, err := AcquireHostMux(0)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if h1.m != h2.m {
		t.Fatal("two acquires must share one mux singleton")
	}
	if got := hostMuxSingleton.refs; got != 2 {
		t.Fatalf("refs = %d, want 2", got)
	}

	// First release: still up (one ref left).
	ReleaseHostMux()
	if hostMuxSingleton.mux == nil {
		t.Fatal("mux torn down while a reference is still held")
	}
	// Last release: torn down.
	ReleaseHostMux()
	if hostMuxSingleton.mux != nil || hostMuxSingleton.refs != 0 {
		t.Fatalf("mux must be torn down on the last release: mux=%v refs=%d", hostMuxSingleton.mux, hostMuxSingleton.refs)
	}
}

// TestHostMuxPortFor_Deterministic: same scope → same port (bookmarkable),
// different scopes generally differ, all within [base, base+span).
func TestHostMuxPortFor_Deterministic(t *testing.T) {
	if HostMuxPortFor("feat-a") != HostMuxPortFor("feat-a") {
		t.Fatal("same scope must derive the same port")
	}
	if HostMuxPortFor("") != hostMuxPortBase {
		t.Fatalf("empty scope must fall back to base %d", hostMuxPortBase)
	}
	for _, s := range []string{"a", "feat-x", "abc123", "deadbeef"} {
		p := HostMuxPortFor(s)
		if p < hostMuxPortBase || p >= hostMuxPortBase+hostMuxPortSpan {
			t.Fatalf("port %d for %q out of range [%d,%d)", p, s, hostMuxPortBase, hostMuxPortBase+hostMuxPortSpan)
		}
	}
}

// TestListenLoopbackFrom_Walk: a taken base port makes the bind walk +1.
func TestListenLoopbackFrom_Walk(t *testing.T) {
	// Occupy an ephemeral port, then ask the walker to start there: it must skip
	// to base+1 (or higher) rather than fail.
	occ, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occ.Close()
	base := occ.Addr().(*net.TCPAddr).Port

	ln, port, err := listenLoopbackFrom(base)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	defer ln.Close()
	if port <= base {
		t.Fatalf("expected a port > taken base %d, got %d", base, port)
	}
}

// TestHostMux_FirstMissDedup: an unknown host warns once; re-registering the
// host re-arms the warning. Case/port variants dedup to one key.
func TestHostMux_FirstMissDedup(t *testing.T) {
	m := newHostMux(0)
	if !m.firstMiss("core.x.localhost") {
		t.Fatal("first miss must report true")
	}
	if m.firstMiss("core.x.localhost") {
		t.Fatal("second miss for same host must be silent")
	}
	if m.firstMiss("Core.X.localhost:8080") {
		t.Fatal("case/port variant must dedup to the same key")
	}
	// Registering the host re-arms the warning (it may later go away again).
	m.register("core.x.localhost", route{})
	if !m.firstMiss("core.x.localhost") {
		t.Fatal("miss after a register must warn again")
	}
}
