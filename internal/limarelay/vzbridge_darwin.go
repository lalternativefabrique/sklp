//go:build darwin && arm64 && cgo

package limarelay

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
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
