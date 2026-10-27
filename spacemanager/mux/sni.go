//go:build linux

package mux

import (
	"crypto/tls"
	"fmt"
	"net"
	"syscall"
	"time"
)

// sni.go peeks the TLS SNI from a ClientHello WITHOUT decrypting or completing
// the handshake, the symmetric twin of peekHost for HTTP. The mux routes the TLS
// connection by SNI (like Host for plain HTTP) and fd-passes the still-intact
// stream to the space's ingress, which terminates TLS. The mux never sees a key
// or a cert — pure passthrough.

// peekSNI reads the ClientHello via MSG_PEEK (non-destructive, so the fd-passed
// stream still carries the full handshake) and returns the SNI server name. It
// loops the peek with a growing view until the ClientHello is complete enough to
// parse, bounded by peekTimeout.
func peekSNI(tcp *net.TCPConn) (string, error) {
	_ = tcp.SetReadDeadline(time.Now().Add(peekTimeout))
	defer tcp.SetReadDeadline(time.Time{})

	raw, err := tcp.SyscallConn()
	if err != nil {
		return "", err
	}

	buf := make([]byte, 4096)
	var sni string
	var parseErr error
	ctrlErr := raw.Read(func(fd uintptr) bool {
		n, _, rerr := syscall.Recvfrom(int(fd), buf, syscall.MSG_PEEK)
		if rerr == syscall.EAGAIN {
			return false // not enough bytes yet; runtime re-polls
		}
		if rerr != nil {
			parseErr = rerr
			return true
		}
		host, ok := sniFromClientHello(buf[:n])
		if !ok {
			// Incomplete ClientHello: wait for more unless the buffer is full.
			if n == len(buf) {
				parseErr = fmt.Errorf("mux: ClientHello too large to peek")
				return true
			}
			return false
		}
		sni = host
		return true
	})
	if ctrlErr != nil {
		return "", ctrlErr
	}
	if parseErr != nil {
		return "", parseErr
	}
	if sni == "" {
		return "", fmt.Errorf("mux: no SNI in ClientHello")
	}
	return sni, nil
}

// sniFromClientHello extracts the SNI from peeked TLS ClientHello bytes. It feeds
// the bytes to a throwaway tls.Server whose GetConfigForClient fires right after
// the ClientHello is parsed; we capture ServerName and abort the handshake. ok is
// false when the bytes don't yet contain a full ClientHello (caller peeks more).
func sniFromClientHello(peeked []byte) (string, bool) {
	var sni string
	conn := &clientHelloConn{data: peeked}
	_ = tls.Server(conn, &tls.Config{
		GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
			sni = h.ServerName
			return nil, errStopHandshake
		},
	}).Handshake()
	if sni == "" {
		return "", false
	}
	return sni, true
}

var errStopHandshake = fmt.Errorf("mux: stop after ClientHello")

// clientHelloConn is a one-shot net.Conn that yields the peeked bytes to the TLS
// parser then EOFs. Writes are discarded (the handshake never proceeds past the
// ClientHello). It is NOT the real connection — only a parser feeder.
type clientHelloConn struct {
	data []byte
	pos  int
}

func (c *clientHelloConn) Read(b []byte) (int, error) {
	if c.pos >= len(c.data) {
		return 0, fmt.Errorf("eof")
	}
	n := copy(b, c.data[c.pos:])
	c.pos += n
	return n, nil
}
func (c *clientHelloConn) Write(b []byte) (int, error)        { return len(b), nil }
func (c *clientHelloConn) Close() error                       { return nil }
func (c *clientHelloConn) LocalAddr() net.Addr                { return nil }
func (c *clientHelloConn) RemoteAddr() net.Addr               { return nil }
func (c *clientHelloConn) SetDeadline(t time.Time) error      { return nil }
func (c *clientHelloConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *clientHelloConn) SetWriteDeadline(t time.Time) error { return nil }
