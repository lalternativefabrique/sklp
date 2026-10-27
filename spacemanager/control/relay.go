package control

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

const relayHelloTimeout = 5 * time.Second

// NodeHostResolver maps a mesh node ID to the address the front can dial its
// control API on. ok=false when the node is unknown to the mesh.
type NodeHostResolver func(nodeID string) (host string, ok bool)

// RelayListener fronts the control API of every private origin: a connection
// whose TLS SNI is "<node-id>.<domain>" is spliced, still encrypted, to that
// node's own control API. The front never terminates it, so the origin's
// pinned certificate and bearer token stay end-to-end. Every other connection
// (no SNI, another name) is handed to the local server through Accept.
type RelayListener struct {
	inner   net.Listener
	domain  string
	port    string
	resolve NodeHostResolver
	dial    func(ctx context.Context, addr string) (net.Conn, error)

	helloTimeout time.Duration

	local  chan net.Conn
	closed chan struct{}
	once   sync.Once
	onErr  func(error)
}

// NewRelayListener wraps inner. domain is the relay suffix without a leading
// dot (e.g. "nodes.front.example.org"); port is the control port dialled on
// the origin, which every sklet exposes identically.
func NewRelayListener(inner net.Listener, domain, port string, resolve NodeHostResolver) *RelayListener {
	return newRelayListener(inner, domain, port, resolve, relayHelloTimeout)
}

func newRelayListener(inner net.Listener, domain, port string, resolve NodeHostResolver, helloTimeout time.Duration) *RelayListener {
	rl := &RelayListener{
		inner:   inner,
		domain:  strings.TrimSuffix(strings.TrimPrefix(domain, "."), "."),
		port:    port,
		resolve: resolve,
		dial: func(ctx context.Context, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		},
		helloTimeout: helloTimeout,
		local:        make(chan net.Conn),
		closed:       make(chan struct{}),
		onErr:        func(error) {},
	}
	go rl.acceptLoop()
	return rl
}

// OnError installs a hook for relay failures (unknown node, dial failure); the
// connection is closed regardless.
func (rl *RelayListener) OnError(fn func(error)) { rl.onErr = fn }

func (rl *RelayListener) acceptLoop() {
	for {
		c, err := rl.inner.Accept()
		if err != nil {
			rl.Close()
			return
		}
		go rl.handle(c)
	}
}

func (rl *RelayListener) handle(c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(rl.helloTimeout))
	sni, buffered, err := peekSNI(c)
	_ = c.SetReadDeadline(time.Time{})
	if err != nil {
		c.Close()
		return
	}
	replay := &replayConn{Conn: c, head: bytes.NewReader(buffered)}

	nodeID, ok := rl.nodeID(sni)
	if !ok {
		select {
		case rl.local <- replay:
		case <-rl.closed:
			c.Close()
		}
		return
	}
	host, ok := rl.resolve(nodeID)
	if !ok {
		c.Close()
		rl.onErr(errors.New("relay: unknown node " + nodeID))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), rl.helloTimeout)
	upstream, err := rl.dial(ctx, net.JoinHostPort(host, rl.port))
	cancel()
	if err != nil {
		c.Close()
		rl.onErr(err)
		return
	}
	splice(replay, upstream)
}

func (rl *RelayListener) nodeID(sni string) (string, bool) {
	if rl.domain == "" || sni == "" {
		return "", false
	}
	sni = strings.TrimSuffix(strings.ToLower(sni), ".")
	suffix := "." + strings.ToLower(rl.domain)
	if !strings.HasSuffix(sni, suffix) {
		return "", false
	}
	id := strings.TrimSuffix(sni, suffix)
	if id == "" || strings.Contains(id, ".") {
		return "", false
	}
	return id, true
}

func (rl *RelayListener) Accept() (net.Conn, error) {
	select {
	case c := <-rl.local:
		return c, nil
	case <-rl.closed:
		return nil, net.ErrClosed
	}
}

func (rl *RelayListener) Close() error {
	var err error
	rl.once.Do(func() {
		close(rl.closed)
		err = rl.inner.Close()
	})
	return err
}

func (rl *RelayListener) Addr() net.Addr { return rl.inner.Addr() }

// peekSNI reads exactly the bytes crypto/tls consumes to parse the
// ClientHello and returns the SNI it carried together with those bytes, so
// the caller can replay them to whichever server ends up handling the
// connection. A hello without SNI yields "".
func peekSNI(c net.Conn) (sni string, buffered []byte, err error) {
	var buf bytes.Buffer
	tee := &teeReadConn{r: io.TeeReader(c, &buf)}
	stop := errors.New("relay: hello parsed")
	cfg := &tls.Config{
		GetConfigForClient: func(hi *tls.ClientHelloInfo) (*tls.Config, error) {
			sni = hi.ServerName
			return nil, stop
		},
	}
	herr := tls.Server(tee, cfg).Handshake()
	if herr == nil || !errors.Is(herr, stop) {
		if herr == nil {
			herr = errors.New("relay: handshake completed unexpectedly")
		}
		return "", nil, herr
	}
	return sni, buf.Bytes(), nil
}

type teeReadConn struct {
	r io.Reader
}

func (t *teeReadConn) Read(p []byte) (int, error)       { return t.r.Read(p) }
func (t *teeReadConn) Write(p []byte) (int, error)      { return len(p), nil }
func (t *teeReadConn) Close() error                     { return nil }
func (t *teeReadConn) LocalAddr() net.Addr              { return nil }
func (t *teeReadConn) RemoteAddr() net.Addr             { return nil }
func (t *teeReadConn) SetDeadline(time.Time) error      { return nil }
func (t *teeReadConn) SetReadDeadline(time.Time) error  { return nil }
func (t *teeReadConn) SetWriteDeadline(time.Time) error { return nil }

type replayConn struct {
	net.Conn
	head *bytes.Reader
}

func (r *replayConn) Read(p []byte) (int, error) {
	if r.head.Len() > 0 {
		return r.head.Read(p)
	}
	return r.Conn.Read(p)
}

type closeWriter interface{ CloseWrite() error }

func splice(a, b net.Conn) {
	var wg sync.WaitGroup
	copyDir := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(closeWriter); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}
	wg.Add(2)
	go copyDir(b, a)
	go copyDir(a, b)
	wg.Wait()
	a.Close()
	b.Close()
}

func (r *replayConn) CloseWrite() error {
	if cw, ok := r.Conn.(closeWriter); ok {
		return cw.CloseWrite()
	}
	return r.Conn.Close()
}
