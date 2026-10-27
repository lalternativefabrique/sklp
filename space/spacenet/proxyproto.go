package spacenet

import (
	"encoding/binary"
	"net"
)

// proxyV2Signature opens every PROXY protocol v2 header (haproxy spec §2.2).
var proxyV2Signature = []byte("\x0D\x0A\x0D\x0A\x00\x0D\x0A\x51\x55\x49\x54\x0A")

const (
	proxyV2CmdLocal = 0x20
	proxyV2CmdProxy = 0x21
	proxyV2TCPv4    = 0x11
	proxyV2TCPv6    = 0x21
)

// proxyProtocolV2Header encodes the client's real endpoints for a backend that
// trusts this proxy. A non-TCP client (a unix socket, a test pair) yields a
// LOCAL header: the backend then keeps the connection's own addresses.
func proxyProtocolV2Header(src, dst net.Addr) []byte {
	s, sok := src.(*net.TCPAddr)
	d, dok := dst.(*net.TCPAddr)
	hdr := append([]byte(nil), proxyV2Signature...)
	if !sok || !dok {
		return append(hdr, proxyV2CmdLocal, 0x00, 0x00, 0x00)
	}
	if s4, d4 := s.IP.To4(), d.IP.To4(); s4 != nil && d4 != nil {
		hdr = append(hdr, proxyV2CmdProxy, proxyV2TCPv4, 0x00, 12)
		hdr = append(hdr, s4...)
		hdr = append(hdr, d4...)
	} else {
		hdr = append(hdr, proxyV2CmdProxy, proxyV2TCPv6, 0x00, 36)
		hdr = append(hdr, s.IP.To16()...)
		hdr = append(hdr, d.IP.To16()...)
	}
	hdr = binary.BigEndian.AppendUint16(hdr, uint16(s.Port))
	hdr = binary.BigEndian.AppendUint16(hdr, uint16(d.Port))
	return hdr
}
