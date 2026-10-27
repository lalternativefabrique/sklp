//go:build linux

package spacenet

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"

	"github.com/vishvananda/netns"
)

// ingress_fs_linux.go exposes the worker's ingress control channel ALSO on a
// filesystem socket, in addition to the inherited fd-pair (startIngressServer).
// The fd-pair only reaches the supervisor process that created the SpaceNet; a
// SEPARATE host process (the gateway MUX, launched by `sklp gateway up`) cannot
// touch it. A deterministic FS socket — the same trick attachSock uses — lets
// the mux dial the worker cross-process and fd-pass accepted client connections
// (SCM_RIGHTS over unixpacket, message boundaries preserved). Additive: the
// inherited-fd path is untouched; this is a second door onto the same serve loop.

// envIngressSock carries the filesystem path of the worker's ingress control
// socket. The worker listens on it; the gateway mux (a separate re-exec/daemon)
// connects to it to push accepted client sockets for in-netns splicing. The
// worker shares the host mount namespace, so the path reaches both processes.
const envIngressSock = "SKLP_SPACE_INGRESS_SOCK"

// listenIngressFS runs in the worker: it listens on the ingress FS socket
// (unixpacket so each SCM_RIGHTS send stays one message) and serves each accepted
// mux connection with the SAME serveIngress loop the inherited fd uses. Returns
// the listener (closed on worker exit) or nil when no path was wired.
func listenIngressFS(capNS netns.NsHandle, names *nameTable) (net.Listener, error) {
	path := os.Getenv(envIngressSock)
	if path == "" {
		return nil, nil
	}
	// A stale socket from a crashed prior worker would block Listen; the path is
	// space-private, so removing it is safe.
	_ = os.Remove(path)
	ln, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: path, Net: "unixpacket"})
	if err != nil {
		return nil, fmt.Errorf("ingress socket %q: %w", path, err)
	}
	go func() {
		for {
			conn, err := ln.AcceptUnix()
			if err != nil {
				return // listener closed: worker exiting
			}
			// serveIngress splices a caller-named service:port from inside the
			// space netns, so a peer from another uid must never reach it.
			if cerr := checkPeerUID(conn); cerr != nil {
				fmt.Fprintf(os.Stderr, "spacenet: ingress: rejected peer: %v\n", cerr)
				_ = conn.Close()
				continue
			}
			// One mux connection = one ctrl channel; serveIngress reads SCM_RIGHTS
			// messages off it until the mux disconnects, then this goroutine ends.
			go serveIngress(capNS, names, conn)
		}
	}()
	return ln, nil
}

// IngressClient is a mux-side connection to one space's ingress FS socket. It
// fd-passes accepted client sockets to that space's worker (same wire format as
// the inherited fd: a JSON ingressRequest + the client fd via SCM_RIGHTS), which
// dials the named service in the netns and splices. One client is reused for
// many connections; it is safe for concurrent SendConn.
type IngressClient struct {
	mu   sync.Mutex
	conn *net.UnixConn
}

// DialIngress connects to a space's ingress FS socket (IngressSockPath). The mux
// derives the path from the space name alone — no shared fd — so it reaches a
// space owned by a DIFFERENT supervisor process. Returns an error if the worker
// is not listening (space down / no FS socket wired).
func DialIngress(path string) (*IngressClient, error) {
	conn, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: path, Net: "unixpacket"})
	if err != nil {
		return nil, fmt.Errorf("dial ingress socket %q: %w", path, err)
	}
	return &IngressClient{conn: conn}, nil
}

// SendConn fd-passes client to the worker, asking it to splice to service:port in
// the space netns. The worker reads the client fd directly, so any bytes already
// in the socket's kernel buffer (the request the mux only PEEKED, never consumed)
// reach the backend intact — no replay needed. The caller's conn is closed here
// either way (the worker holds its own dup). Concurrency-safe.
func (c *IngressClient) SendConn(service string, port int, client net.Conn) error {
	tcp, ok := client.(*net.TCPConn)
	if !ok {
		_ = client.Close()
		return fmt.Errorf("ingress client: expected *net.TCPConn, got %T", client)
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		_ = client.Close()
		return fmt.Errorf("ingress client: raw conn: %w", err)
	}
	payload, err := json.Marshal(ingressRequest{Service: service, Port: port})
	if err != nil {
		_ = client.Close()
		return err
	}
	var sendErr error
	ctrlErr := raw.Control(func(fd uintptr) {
		rights := syscall.UnixRights(int(fd))
		c.mu.Lock()
		_, _, sendErr = c.conn.WriteMsgUnix(payload, rights, nil)
		c.mu.Unlock()
	})
	_ = client.Close() // worker holds its own dup; never leak the host copy
	if ctrlErr != nil {
		return fmt.Errorf("ingress client: control: %w", ctrlErr)
	}
	if sendErr != nil {
		return fmt.Errorf("ingress client: send: %w", sendErr)
	}
	return nil
}

// Close drops the mux's control connection to this space's worker.
func (c *IngressClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.Close()
}
