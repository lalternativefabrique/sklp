package space

import (
	"fmt"
	"net"
)

// allocLoopbackPort asks the kernel for a free loopback TCP port by binding
// 127.0.0.1:0, reading the assigned port, then closing the listener. It is the
// standard Go idiom for "give me a port nobody else holds right now".
//
// TOCTOU: there is a tiny window between Close here and the caller re-binding
// that exact port — another process could grab it in between. The gateway
// ingress (OpenGatewayIngress) re-binds the returned port IMMEDIATELY, so the
// window is sub-millisecond. On a single-host dev/deploy box with a handful of
// spaces this is acceptable; a collision surfaces as a logged ingress bind
// failure (hostIngress.OpenService is best-effort), not data loss. A future
// hardening could keep the listener open and hand its fd to the ingress, but
// that is more plumbing than this single-host case warrants.
func allocLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(ingressBindHost, "0"))
	if err != nil {
		return 0, fmt.Errorf("alloc loopback port: %w", err)
	}
	defer ln.Close()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("alloc loopback port: unexpected addr type %T", ln.Addr())
	}
	return addr.Port, nil
}
