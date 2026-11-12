//go:build darwin && arm64 && cgo

package limarelay

import (
	"fmt"
	"io"
	"net"
	"os"
	"sync"
)

// ForwardHostPorts publishes guest ports on the Mac's loopback for as long as
// the caller keeps them: a dev stack's declared ports and its host mux port.
// Nothing is published ahead of time, so a port the stack does not use stays
// free for the Mac's own tools (Metro on 8081, adb on 5037). A port something
// already holds is left to it and named. spare ports are published the same way
// but silently when taken: they only matter if the guest lands on one. Does
// nothing when no VM daemon serves forwards.
func ForwardHostPorts(ports, spare []int) (release func()) {
	sock := vzForwardSocketPath()
	if !socketAlive(sock) {
		return func() {}
	}
	var listeners []net.Listener
	listen := func(port int, announce bool) {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			if announce {
				fmt.Fprintf(os.Stderr, "[sklp] macos: 127.0.0.1:%d is already taken on the Mac; the stack's port %d is not published there\n", port, port)
			}
			return
		}
		listeners = append(listeners, ln)
		go acceptForwards(ln, sock, port)
	}
	for _, port := range ports {
		listen(port, true)
	}
	for _, port := range spare {
		listen(port, false)
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
