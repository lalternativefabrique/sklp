//go:build darwin && arm64 && cgo

package limarelay

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
)

// forwardPort is the guest agent's forwarding listener. Keep in sync with
// cmd/sklp-vzagent.
const forwardPort = 1025

// Ports a service binds inside the VM are unreachable from the Mac: the NAT
// attachment routes outbound only. The daemon therefore listens on the host's
// loopback for each forwarded port and splices every accepted connection to
// the same port inside the guest — so http://127.0.0.1:3000 in the browser
// reaches the dev server the stack just printed a URL for.
type portForward struct {
	port   int
	ln     net.Listener
	warned atomic.Bool
}

// warnUnreachable names the port whose guest side could not be reached, once.
func (f *portForward) warnUnreachable(err error) {
	if f.warned.CompareAndSwap(false, true) {
		fmt.Fprintf(os.Stderr, "[sklp] macos vm: :%d has no guest listener (%v)\n", f.port, err)
	}
}

// forwardHostPort starts listening on 127.0.0.1:port and relays into the
// guest. Returns a closer; a port already taken on the host is reported, not
// silently skipped, since the developer would otherwise be told a URL that
// answers with something else entirely.
func forwardHostPort(port int) (*portForward, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("forward :%d: %w", port, err)
	}
	f := &portForward{port: port, ln: ln}
	go f.accept()
	return f, nil
}

func (f *portForward) accept() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return // listener closed
		}
		go f.pipe(conn)
	}
}

// pipe opens one guest-side connection per host connection: the agent expects
// a fresh vsock session per forwarded stream, carrying its target port.
//
// A forward whose guest side cannot be reached closes the accepted connection
// having written nothing, which a browser reports as ERR_EMPTY_RESPONSE — a
// symptom that says nothing about the cause. The ports are published
// speculatively (see defaultForwardPorts), so an unreachable guest side is the
// normal state for most of them, not an anomaly: warn once per port to name it
// without spamming a reload loop.
func (f *portForward) pipe(host net.Conn) {
	defer host.Close()

	guest, err := dialGuestPort(forwardPort)
	if err != nil {
		f.warnUnreachable(err)
		return
	}
	defer guest.Close()

	// Marshal, not Encoder: Encode appends a newline the agent would replay
	// ahead of the client's bytes, and Go's net/http answers a request that
	// starts with an empty line with a bare 400.
	head, err := json.Marshal(forwardRequest{Port: f.port})
	if err != nil {
		f.warnUnreachable(err)
		return
	}
	if _, err := guest.Write(head); err != nil {
		f.warnUnreachable(err)
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	var up, down int64
	var upErr, downErr error
	go func() {
		defer wg.Done()
		up, upErr = io.Copy(guest, host)
		if cw, ok := guest.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		down, downErr = io.Copy(host, guest)
		if cw, ok := host.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	wg.Wait()
	if debugForwards {
		fmt.Fprintf(os.Stderr, "[sklp] fwd :%d up=%d(%v) down=%d(%v)\n", f.port, up, upErr, down, downErr)
	}
}

// debugForwards traces every spliced connection's byte counts to stderr.
// SKLP_DEBUG_FWD=1; per-connection lines are far too chatty to keep on.
var debugForwards = os.Getenv("SKLP_DEBUG_FWD") == "1"

func (f *portForward) Close() error { return f.ln.Close() }

type forwardRequest struct {
	Port int `json:"port"`
}

// defaultForwardPorts are published without being asked for. The host mux
// range is what `sklp dev` prints its ingress URLs against, and 3000/5273 are
// the dev servers a stack binds directly.
//
// Announcing a fixed set is a stopgap: the honest fix is to publish what the
// guest actually listens on, which needs the runtime to report its bindings.
func defaultForwardPorts() []int {
	ports := []int{3000, 4100, 5273, 4000, 5173}
	// The per-worktree host mux port is derived by hashing the scope id into
	// [8080, 9080), and the daemon does not know which worktree a later command
	// will run in — so the whole range is published. Listening on a loopback
	// port is a socket and an accept goroutine, nothing more, and the ones
	// already taken on the host are skipped.
	for p := hostMuxPortBase; p < hostMuxPortBase+hostMuxPortSpan; p++ {
		ports = append(ports, p)
	}
	return ports
}

// Kept in sync with space.hostMuxPortBase / hostMuxPortSpan, which cannot be
// imported here: that package is linux-only.
const (
	hostMuxPortBase = 8080
	hostMuxPortSpan = 1000
)

// startForwards publishes the default ports, reporting the ones already taken
// on the host instead of failing the VM over them.
func startForwards() []*portForward {
	var out []*portForward
	for _, p := range defaultForwardPorts() {
		f, err := forwardHostPort(p)
		if err != nil {
			continue // taken on the host: leave whatever owns it alone
		}
		out = append(out, f)
	}
	fmt.Fprintf(os.Stderr, "[sklp] macos vm: forwarding %d ports from the guest\n", len(out))
	return out
}
