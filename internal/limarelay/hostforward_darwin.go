//go:build darwin && arm64 && cgo

package limarelay

import (
	"fmt"
	"io"
	"net"
	"sync"
)

// ForwardHostPorts publishes guest ports on the Mac's loopback for as long as
// the caller keeps them: a dev stack's declared ports, which the daemon's own
// list cannot know. A port the daemon or anything else already holds is left
// alone. Does nothing when no VM daemon serves forwards.
func ForwardHostPorts(ports []int) (release func()) {
	sock := vzForwardSocketPath()
	if !socketAlive(sock) {
		return func() {}
	}
	var listeners []net.Listener
	for _, port := range ports {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		listeners = append(listeners, ln)
		go acceptForwards(ln, sock, port)
	}
	return func() {
		for _, ln := range listeners {
			_ = ln.Close()
		}
	}
}

func acceptForwards(ln net.Listener, sock string, port int) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go spliceToGuest(conn, sock, port)
	}
}

func spliceToGuest(host net.Conn, sock string, port int) {
	defer host.Close()
	guest, err := net.Dial("unix", sock)
	if err != nil {
		return
	}
	defer guest.Close()
	if _, err := fmt.Fprintf(guest, "%d\n", port); err != nil {
		return
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(guest, host)
		if cw, ok := guest.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(host, guest)
		if cw, ok := host.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	wg.Wait()
}
