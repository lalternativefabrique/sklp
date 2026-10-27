package spacenet

import (
	"bytes"
	"net"
	"testing"
)

func TestProxyProtocolV2HeaderTCPv4(t *testing.T) {
	src := &net.TCPAddr{IP: net.IPv4(203, 0, 113, 7), Port: 51234}
	dst := &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 25}
	got := proxyProtocolV2Header(src, dst)
	want := append(append([]byte(nil), proxyV2Signature...),
		0x21, 0x11, 0x00, 12,
		203, 0, 113, 7,
		10, 0, 0, 1,
		0xC8, 0x22,
		0x00, 25)
	if !bytes.Equal(got, want) {
		t.Fatalf("header = % x\nwant     % x", got, want)
	}
}

func TestProxyProtocolV2HeaderTCPv6(t *testing.T) {
	src := &net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 4000}
	dst := &net.TCPAddr{IP: net.ParseIP("2001:db8::2"), Port: 443}
	got := proxyProtocolV2Header(src, dst)
	if len(got) != 16+36 {
		t.Fatalf("len = %d, want 52", len(got))
	}
	if got[12] != 0x21 || got[13] != 0x21 || got[15] != 36 {
		t.Fatalf("prefix = % x", got[12:16])
	}
	if !bytes.Equal(got[16:32], src.IP.To16()) || !bytes.Equal(got[32:48], dst.IP.To16()) {
		t.Fatalf("addresses not encoded: % x", got[16:48])
	}
	if got[48] != 0x0F || got[49] != 0xA0 || got[50] != 0x01 || got[51] != 0xBB {
		t.Fatalf("ports = % x", got[48:])
	}
}

func TestProxyProtocolV2HeaderNonTCPIsLocal(t *testing.T) {
	got := proxyProtocolV2Header(&net.UnixAddr{Name: "@x", Net: "unix"}, &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 25})
	want := append(append([]byte(nil), proxyV2Signature...), 0x20, 0x00, 0x00, 0x00)
	if !bytes.Equal(got, want) {
		t.Fatalf("header = % x, want LOCAL % x", got, want)
	}
}
