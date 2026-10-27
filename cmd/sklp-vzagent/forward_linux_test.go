//go:build linux

package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestForwardSessionReplaysClientBytesWithoutTheRequestNewline(t *testing.T) {
	for name, head := range map[string]string{
		"host without newline": `{"port":%d}`,
		"older host newline":   "{\"port\":%d}\n",
	} {
		t.Run(name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			const request = "GET /health HTTP/1.1\r\nHost: x\r\n\r\n"
			got := make(chan string, 1)
			go func() {
				c, err := ln.Accept()
				if err != nil {
					got <- err.Error()
					return
				}
				defer c.Close()
				buf := make([]byte, len(request))
				_, _ = io.ReadFull(c, buf)
				got <- string(buf)
			}()

			fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
			if err != nil {
				t.Fatal(err)
			}
			host := os.NewFile(uintptr(fds[0]), "host")
			defer host.Close()
			go forwardSession(os.NewFile(uintptr(fds[1]), "agent"))

			port := ln.Addr().(*net.TCPAddr).Port
			if _, err := host.Write([]byte(fmt.Sprintf(head, port) + request)); err != nil {
				t.Fatal(err)
			}
			select {
			case s := <-got:
				if s != request {
					t.Errorf("target received %q, want %q", s, request)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("target never received the request")
			}
		})
	}
}
