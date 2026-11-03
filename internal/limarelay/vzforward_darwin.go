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
// attachment routes outbound only. A forward splices one host connection to the
// same port inside the guest — so http://127.0.0.1:3000 in the browser reaches
// the dev server the stack just printed a URL for.
type portForward struct {
	port   int
	warned atomic.Bool
}

// warnUnreachable names the port whose guest side could not be reached, once.
func (f *portForward) warnUnreachable(err error) {
	if f.warned.CompareAndSwap(false, true) {
		fmt.Fprintf(os.Stderr, "[sklp] macos vm: :%d has no guest listener (%v)\n", f.port, err)
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

type forwardRequest struct {
	Port int `json:"port"`
}
