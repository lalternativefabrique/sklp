//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// Services in the VM bind ports the developer expects to reach from the Mac:
// a Vite dev server on :3000, the space's host mux on :8940. The VM's NAT
// attachment only routes outbound, so nothing on the host can dial in.
//
// This is the way back: a second vsock port whose only job is to be told a
// guest TCP port and then splice the two streams. The host end listens on
// 127.0.0.1 and hands each accepted connection here, so from the browser it
// looks like the service is listening locally — which is what every URL the
// stack prints already claims.
const forwardPort = 1025

// forwardRequest is the one-line preamble a forwarding session starts with.
type forwardRequest struct {
	Port int `json:"port"`
}

// serveForward accepts forwarding sessions for the life of the VM.
func serveForward() {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		logf("FWD: socket: %v", err)
		return
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: forwardPort}); err != nil {
		logf("FWD: bind: %v", err)
		return
	}
	if err := unix.Listen(fd, 32); err != nil {
		logf("FWD: listen: %v", err)
		return
	}
	logf("FWD: listening on vsock:%d", forwardPort)

	for {
		nfd, _, err := unix.Accept(fd)
		if err != nil {
			logf("FWD: accept: %v", err)
			continue
		}
		go forwardSession(os.NewFile(uintptr(nfd), "vsock-fwd"))
	}
}

// forwardSession reads which guest port the host wants, dials it, and copies
// bytes both ways until either side closes.
func forwardSession(conn *os.File) {
	defer conn.Close()

	// The decoder reads ahead: client bytes that arrive coalesced with the
	// request line (a WebSocket upgrade sent the instant the stream opens)
	// land in its buffer and MUST be replayed to the target, or the backend
	// waits forever for a request whose start was swallowed.
	dec := json.NewDecoder(conn)
	var req forwardRequest
	if err := dec.Decode(&req); err != nil {
		logf("FWD: decode: %v", err)
		return
	}
	if req.Port <= 0 || req.Port > 65535 {
		return
	}
	buffered, err := io.ReadAll(dec.Buffered())
	if err != nil {
		return
	}
	// An older host ends the request with json.Encoder's newline; replayed, it
	// makes Go's net/http reject the client's request line with a 400.
	buffered = bytes.TrimPrefix(buffered, []byte("\n"))

	// 127.0.0.1 rather than the bridge address: a service that binds only
	// loopback inside the guest is exactly the case this exists for.
	target, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", req.Port))
	if err != nil {
		logf("FWD: dial :%d: %v", req.Port, err)
		return
	}
	defer target.Close()
	if len(buffered) > 0 {
		if _, err := target.Write(buffered); err != nil {
			return
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(target, conn)
		if cw, ok := target.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(conn, target)
	}()
	wg.Wait()
}
