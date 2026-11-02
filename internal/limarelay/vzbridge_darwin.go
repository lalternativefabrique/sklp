//go:build darwin && arm64 && cgo

package limarelay

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A VZVirtualMachine can only be reached from the process that created it:
// connectToPort is a method on the VM object, not something a second process
// can call. So the daemon that owns the VM also fronts it — it listens on a
// UNIX socket, and for each client that connects it opens a fresh vsock
// session to the guest and splices the two together.
//
// The bridge is deliberately dumb: it copies bytes. The request/response
// framing stays exactly what host and guest agent already speak, so nothing
// about the protocol changes by going through a daemon.

func vzSocketPath() string { return filepath.Join(vzRunDir(), "vm.sock") }

// vzForwardSocketPath is the daemon's second socket: a client names a guest
// port on its first line and is then spliced to it, which is how a command on
// the Mac publishes a port the daemon's fixed list does not carry.
func vzForwardSocketPath() string { return filepath.Join(vzRunDir(), "vm-fwd.sock") }

// serveForwardBridge accepts forward clients until stop closes.
func serveForwardBridge(stop <-chan struct{}) error {
	path := vzForwardSocketPath()
	if socketAlive(path) {
		return errVMBusy
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return err
	}
	go func() {
		<-stop
		ln.Close()
		_ = os.Remove(path)
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-stop:
				return nil
			default:
			}
			return err
		}
		go forwardSession(conn)
	}
}

// forwardSession reads the guest port a client asks for, then splices the
// rest of the connection to that port through the agent's forwarder.
func forwardSession(client net.Conn) {
	port, rest, err := readForwardHead(client)
	if err != nil {
		client.Close()
		return
	}
	f := &portForward{port: port}
	f.pipe(prefixedConn{Conn: client, Reader: rest})
}

// readForwardHead parses the port line a forward client sends first. The
// bytes read past the newline belong to the stream and are handed back.
func readForwardHead(r io.Reader) (int, io.Reader, error) {
	br := bufio.NewReader(r)
	line, err := br.ReadString('\n')
	if err != nil {
		return 0, nil, err
	}
	port, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || port <= 0 || port > 65535 {
		return 0, nil, fmt.Errorf("forward: bad port line %q", line)
	}
	return port, br, nil
}

// prefixedConn reads from the buffered reader that consumed the head, and
// otherwise behaves as the connection it wraps.
type prefixedConn struct {
	net.Conn
	io.Reader
}

func (c prefixedConn) Read(p []byte) (int, error) { return c.Reader.Read(p) }

func (c prefixedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// serveBridge accepts client sessions for as long as ctx-like stop is open.
// Each one gets its own vsock connection to the guest, so a long-running
// `sklp dev` does not block a cockpit opening beside it.
func serveBridge(stop <-chan struct{}) error {
	path := vzSocketPath()
	// A socket left behind by a crashed daemon would refuse the bind. Probe it
	// first: only remove it when nobody answers, so a live daemon is never
	// unseated by a racing start.
	if socketAlive(path) {
		return errVMBusy
	}
	_ = os.Remove(path)

	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return err
	}

	go func() {
		<-stop
		ln.Close()
		_ = os.Remove(path)
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-stop:
				return nil
			default:
			}
			return err
		}
		go bridgeSession(conn)
	}
}

// socketAlive reports whether something is already serving at path. A short
// timeout: this only distinguishes a live daemon from a leftover file.
func socketAlive(path string) bool {
	if _, err := os.Stat(path); err != nil {
		return false
	}
	c, err := net.DialTimeout("unix", path, 200*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// bridgeSession wires one client to one guest session, copying in both
// directions until either end closes.
func bridgeSession(client net.Conn) {
	defer client.Close()

	guest, err := dialGuest()
	if err != nil {
		fmt.Fprintf(client, "X{\"exit\":1,\"err\":%q}\n", err.Error())
		return
	}
	defer guest.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(guest, client)
		// Half-close so the guest sees EOF on its side and can finish.
		if cw, ok := guest.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(client, guest)
		if cw, ok := client.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	wg.Wait()
}
