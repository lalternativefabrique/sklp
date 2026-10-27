//go:build linux

package mux

import (
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/space/spacenet"
)

// fakeWorker listens on a space's ingress FS socket, receives one fd-passed
// connection + its ingressRequest, reads the full HTTP request off the passed
// fd, and reports what it saw. It stands in for the real space worker so the mux
// is tested end-to-end (peek + route + cross-process fd-pass) without a netns.
type fakeWorker struct {
	ln   *net.UnixListener
	got  chan workerObservation
	path string
}

type workerObservation struct {
	service string
	port    int
	request string
}

func startFakeWorker(t *testing.T, space string) *fakeWorker {
	t.Helper()
	path := spacenet.IngressSockPath(space)
	_ = os.Remove(path)
	ln, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: path, Net: "unixpacket"})
	if err != nil {
		t.Fatalf("fake worker listen: %v", err)
	}
	w := &fakeWorker{ln: ln, got: make(chan workerObservation, 1), path: path}
	go w.serve()
	return w
}

func (w *fakeWorker) serve() {
	conn, err := w.ln.AcceptUnix()
	if err != nil {
		return
	}
	defer conn.Close()
	buf := make([]byte, 1024)
	oob := make([]byte, syscall.CmsgSpace(4))
	n, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
	if err != nil {
		return
	}
	scms, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(scms) == 0 {
		return
	}
	fds, err := syscall.ParseUnixRights(&scms[0])
	if err != nil || len(fds) == 0 {
		return
	}
	// The mux only PEEKed: the passed fd must still hold the full request.
	f := os.NewFile(uintptr(fds[0]), "passed-client")
	defer f.Close()
	reqBuf := make([]byte, 512)
	_ = f.SetReadDeadline(time.Now().Add(time.Second))
	m, _ := f.Read(reqBuf)

	// Parse the ingressRequest the mux sent (service + port) from the JSON payload.
	svc, port := parseIngressPayload(buf[:n])
	w.got <- workerObservation{service: svc, port: port, request: string(reqBuf[:m])}
}

// parseIngressPayload is a tiny JSON pluck so the test does not import the
// unexported ingressRequest type. Format: {"service":"sklp-ingress","port":8000}
func parseIngressPayload(b []byte) (string, int) {
	var svc string
	var port int
	_, _ = fmt.Sscanf(string(b), `{"service":%q,"port":%d}`, &svc, &port)
	return svc, port
}

func (w *fakeWorker) close() {
	_ = w.ln.Close()
	_ = os.Remove(w.path)
}

func TestMux_RoutesAndFdPasses_PeekNonDestructive(t *testing.T) {
	const space = "muxtest-space"
	w := startFakeWorker(t, space)
	defer w.close()

	m := New()
	if err := m.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	// Option 4: a plain Host route targets the service DIRECTLY (no ingress hop).
	m.SetRoutes(map[string]Target{"nginx.test": {Space: space, Service: "nginx", Port: 80}})
	go m.Serve()

	// Client hits the mux with a Host the route table knows.
	conn, err := net.Dial("tcp", m.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	req := "GET /hello HTTP/1.1\r\nHost: nginx.test\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}

	select {
	case obs := <-w.got:
		if obs.service != "nginx" {
			t.Errorf("service = %q, want %q (direct service routing)", obs.service, "nginx")
		}
		if obs.port != 80 {
			t.Errorf("port = %d, want 80", obs.port)
		}
		// THE proof: the worker read the FULL request off the passed fd — the mux
		// peeked the Host without consuming it.
		if obs.request != req {
			t.Errorf("worker saw %q, want full request %q (peek consumed bytes!)", obs.request, req)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker never received the fd-passed connection")
	}
}

func TestMux_RoutesBySNI_PassthroughIntact(t *testing.T) {
	const space = "muxtest-tls-space"
	w := startFakeWorker(t, space)
	defer w.close()

	m := New()
	if err := m.ListenTLS("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	// The mux SNI-routes every HTTPS connection to the space's native L7 TLS
	// endpoint (which terminates TLS); the per-route Service is overridden, only
	// the SPACE matters. The route's declared service is ignored for HTTPS.
	m.SetRoutes(map[string]Target{"secure.test": {Space: space, Service: "anything", Port: 1}})
	go m.Serve()

	// A TLS client whose ClientHello carries SNI=secure.test. The handshake won't
	// complete (the fake worker doesn't speak TLS), but the mux must have peeked
	// the SNI and fd-passed the ClientHello intact BEFORE that.
	addr := m.AddrTLS().String()
	go func() {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			return
		}
		_ = tls.Client(conn, &tls.Config{
			ServerName:         "secure.test",
			InsecureSkipVerify: true,
		}).Handshake()
	}()

	select {
	case obs := <-w.got:
		if obs.service != "sklp-l7-tls" || obs.port != 8443 {
			t.Errorf("routed to %s:%d, want sklp-l7-tls:8443 (native L7 TLS)", obs.service, obs.port)
		}
		// The passed fd must carry the TLS record (handshake type 22 = 0x16).
		if len(obs.request) == 0 || obs.request[0] != 0x16 {
			t.Errorf("passed stream is not a TLS ClientHello (first byte %#x), peek consumed bytes", firstByte(obs.request))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker never received the SNI-routed connection")
	}
}

func firstByte(s string) byte {
	if len(s) == 0 {
		return 0
	}
	return s[0]
}

func TestMux_UnknownHost_Dropped(t *testing.T) {
	m := New()
	if err := m.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.SetRoutes(map[string]Target{"known.test": {Space: "some-space", Service: "svc", Port: 80}})
	go m.Serve()

	conn, err := net.Dial("tcp", m.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Unknown host: the mux must close the connection (no route).
	_, _ = conn.Write([]byte("GET / HTTP/1.1\r\nHost: unknown.test\r\n\r\n"))
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Error("expected the connection to be closed for an unknown host")
	}
}
