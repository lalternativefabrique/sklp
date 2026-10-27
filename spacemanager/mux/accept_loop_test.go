package mux

import (
	"errors"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// flakyListener fails the first N Accepts with EMFILE, then hands over the
// connections a real TCP listener produces. It stands in for a host that ran
// out of descriptors under load and then recovered.
type flakyListener struct {
	real     net.Listener
	failures atomic.Int32
	accepted atomic.Int32
	served   chan struct{}
	once     atomic.Bool
}

func (l *flakyListener) Accept() (net.Conn, error) {
	if l.failures.Add(-1) >= 0 {
		return nil, &net.OpError{Op: "accept", Err: syscall.EMFILE}
	}
	c, err := l.real.Accept()
	if err != nil {
		return nil, err
	}
	l.accepted.Add(1)
	if l.once.CompareAndSwap(false, true) {
		close(l.served)
	}
	return c, nil
}
func (l *flakyListener) Close() error   { return l.real.Close() }
func (l *flakyListener) Addr() net.Addr { return l.real.Addr() }

// The accept loop must SURVIVE descriptor exhaustion and go on serving. Before
// the fix it returned on the first EMFILE, taking the front door of every space
// down permanently over a condition that clears in milliseconds.
func TestAcceptLoop_SurvivesDescriptorExhaustion(t *testing.T) {
	real, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer real.Close()

	ln := &flakyListener{real: real, served: make(chan struct{})}
	ln.failures.Store(5)

	m := &Mux{}
	errc := make(chan error, 1)
	peek := func(*net.TCPConn) (string, error) { return "", errors.New("test: no route") }
	go func() { errc <- m.acceptLoop(ln, peek, false) }()

	// Dial once the loop has had time to burn through the EMFILE failures.
	go func() {
		time.Sleep(400 * time.Millisecond)
		if c, derr := net.Dial("tcp", real.Addr().String()); derr == nil {
			defer c.Close()
			time.Sleep(200 * time.Millisecond)
		}
	}()

	select {
	case <-ln.served:
		// A connection was accepted AFTER five EMFILE failures: the loop held.
	case err := <-errc:
		t.Fatalf("accept loop gave up on a temporary error (%v); the front door would stay down until a restart", err)
	case <-time.After(10 * time.Second):
		t.Fatal("accept loop made no progress after the exhaustion cleared")
	}
}
