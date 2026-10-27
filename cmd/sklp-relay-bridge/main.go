// sklp-relay-bridge makes the same-host Space-to-Space relay (ADR-007)
// transparent to an ordinary TCP client. A standard client — the NATS SDK,
// psql, mongosh, anything that just dials host:port — knows nothing about
// the relay's one-line preamble protocol; this bridge is the one piece that
// does, so nothing else has to.
//
// It listens on a local port and, for every accepted connection, dials the
// calling Space's relay listener (its own gateway IP, port 8002), writes
// the "targetSpace/targetService:targetPort\n" preamble, then splices the
// two connections raw. Protocol-agnostic: once the preamble is sent, every
// byte in either direction passes through unmodified — this works
// identically for NATS, Postgres, MongoDB, or any other TCP protocol.
//
// Usage: sklp-relay-bridge -listen :4222 -target space-nats/nats:4222
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
)

// relayGatewayAddr is the calling Space's relay listener — its own gateway
// IP, fixed relayPort (space/spacenet/relay_linux.go). A bridge
// only ever needs to reach ITS OWN Space's relay: the ACL and the "which
// Space am I" question are both resolved on that end, from depends_on.
const relayGatewayAddr = "10.88.0.1:8002"

func main() {
	listen := flag.String("listen", "", "local address to listen on, e.g. :4222 or 127.0.0.1:5432")
	target := flag.String("target", "", "relay target as space/service:port, e.g. space-nats/nats:4222")
	flag.Parse()

	if *listen == "" || *target == "" {
		fmt.Fprintln(os.Stderr, "usage: sklp-relay-bridge -listen <addr> -target <space>/<service>:<port>")
		os.Exit(2)
	}
	if err := validateTarget(*target); err != nil {
		fmt.Fprintln(os.Stderr, "sklp-relay-bridge:", err)
		os.Exit(2)
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp-relay-bridge: listen:", err)
		os.Exit(1)
	}
	defer ln.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	fmt.Printf("sklp-relay-bridge: %s -> %s (via %s)\n", *listen, *target, relayGatewayAddr)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return // shutting down
			}
			fmt.Fprintln(os.Stderr, "sklp-relay-bridge: accept:", err)
			continue
		}
		go handleConn(conn, *target)
	}
}

// validateTarget rejects an obviously malformed target early, at startup,
// rather than failing silently per-connection later.
func validateTarget(target string) error {
	spaceSvc, port, ok := strings.Cut(target, ":")
	if !ok || port == "" {
		return fmt.Errorf("target %q must be space/service:port", target)
	}
	if !strings.Contains(spaceSvc, "/") {
		return fmt.Errorf("target %q must be space/service:port", target)
	}
	return nil
}

// handleConn dials this Space's relay listener, sends the preamble, then
// splices client and relay raw. A dial or write failure closes the client
// immediately — the caller sees exactly what it would see if the target
// were simply unreachable.
func handleConn(client net.Conn, target string) {
	defer client.Close()

	relay, err := net.Dial("tcp", relayGatewayAddr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp-relay-bridge: dial relay:", err)
		return
	}
	defer relay.Close()

	if _, err := fmt.Fprintf(relay, "%s\n", target); err != nil {
		fmt.Fprintln(os.Stderr, "sklp-relay-bridge: send preamble:", err)
		return
	}

	splice(client, relay)
}

// splice copies bytes in both directions until both sides have finished. A
// source EOF half-closes the destination's write side rather than closing
// the whole connection outright — a client done sending before it starts
// reading (e.g. `nc < file`) would otherwise have its still-arriving
// response severed the instant its own request finished sending.
func splice(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		closeWrite(dst)
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
	_ = a.Close()
	_ = b.Close()
}

// closeWrite half-closes dst's write side if it supports it (both conns
// here are *net.TCPConn), falling back to a full Close otherwise.
func closeWrite(dst net.Conn) {
	type writeCloser interface{ CloseWrite() error }
	if wc, ok := dst.(writeCloser); ok {
		_ = wc.CloseWrite()
		return
	}
	_ = dst.Close()
}
