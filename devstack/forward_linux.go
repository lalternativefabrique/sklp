//go:build linux

package devstack

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lalternativefabrique/sklp/space/spacenet"
)

func forwardToSpace(spaceName string, supervisorPID int, t forwardTarget) int {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", t.HostPort))
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			fmt.Fprintf(os.Stderr, "sklp dev forward: 127.0.0.1:%d is already in use (`ss -ltnp 'sport = :%d'` shows the holder); omit the host port to get a free one\n", t.HostPort, t.HostPort)
		} else {
			fmt.Fprintf(os.Stderr, "sklp dev forward: listen: %v\n", err)
		}
		return 1
	}
	sockPath := spacenet.IngressSockPath(spaceName)
	cl, err := spacenet.DialIngress(sockPath)
	if err != nil {
		_ = ln.Close()
		fmt.Fprintf(os.Stderr, "sklp dev forward: space %s unreachable: %v\n", spaceName, err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	stackGone := make(chan struct{})
	go func() {
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if pidDead(supervisorPID) {
					close(stackGone)
					return
				}
			}
		}
	}()
	go func() {
		select {
		case <-ctx.Done():
		case <-stackGone:
		}
		_ = ln.Close()
	}()

	fmt.Printf("↦ %s:%d → %s (Ctrl-C to stop)\n", t.Service, t.Port, ln.Addr())
	for {
		conn, err := ln.Accept()
		if err != nil {
			break
		}
		if err := cl.SendConn(t.Service, t.Port, conn); err != nil {
			_ = cl.Close()
			if cl, err = spacenet.DialIngress(sockPath); err != nil {
				fmt.Fprintf(os.Stderr, "sklp dev forward: space %s unreachable: %v\n", spaceName, err)
				_ = ln.Close()
				return 1
			}
		}
	}
	_ = cl.Close()
	select {
	case <-stackGone:
		fmt.Fprintln(os.Stderr, "sklp dev forward: the dev stack stopped")
		return 1
	default:
		return 0
	}
}

func relayForward(string, string, forwardTarget) (int, bool) { return 0, false }
