//go:build linux

package spacenet

import (
	"encoding/binary"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func listenUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	return c
}

func echoWithPrefix(t *testing.T, prefix string) *net.UDPConn {
	t.Helper()
	srv := listenUDP(t)
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := srv.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = srv.WriteToUDP(append([]byte(prefix), buf[:n]...), from)
		}
	}()
	return srv
}

func roundTrip(t *testing.T, c *net.UDPConn, to net.Addr, msg string) string {
	t.Helper()
	if _, err := c.WriteTo([]byte(msg), to); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 2048)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("read reply to %q: %v", msg, err)
	}
	return string(buf[:n])
}

func TestRelayUDPRoutesRepliesToTheirClient(t *testing.T) {
	backend := echoWithPrefix(t, "echo:")
	defer backend.Close()
	pub := listenUDP(t)
	var dials atomic.Int32
	dial := func() (*net.UDPConn, error) {
		dials.Add(1)
		return net.DialUDP("udp", nil, backend.LocalAddr().(*net.UDPAddr))
	}
	go relayUDP(pub, dial, time.Minute, 16)
	defer pub.Close()

	a, b := listenUDP(t), listenUDP(t)
	defer a.Close()
	defer b.Close()
	if got := roundTrip(t, a, pub.LocalAddr(), "a1"); got != "echo:a1" {
		t.Fatalf("client a got %q", got)
	}
	if got := roundTrip(t, b, pub.LocalAddr(), "b1"); got != "echo:b1" {
		t.Fatalf("client b got %q", got)
	}
	if got := roundTrip(t, a, pub.LocalAddr(), "a2"); got != "echo:a2" {
		t.Fatalf("client a second datagram got %q", got)
	}
	if n := dials.Load(); n != 2 {
		t.Fatalf("backend sockets opened = %d, want one per client (2)", n)
	}
}

func TestRelayUDPDropsIdleFlows(t *testing.T) {
	backend := echoWithPrefix(t, "")
	defer backend.Close()
	pub := listenUDP(t)
	var dials atomic.Int32
	dial := func() (*net.UDPConn, error) {
		dials.Add(1)
		return net.DialUDP("udp", nil, backend.LocalAddr().(*net.UDPAddr))
	}
	go relayUDP(pub, dial, 100*time.Millisecond, 16)
	defer pub.Close()

	c := listenUDP(t)
	defer c.Close()
	roundTrip(t, c, pub.LocalAddr(), "x")
	time.Sleep(300 * time.Millisecond)
	roundTrip(t, c, pub.LocalAddr(), "y")
	if n := dials.Load(); n != 2 {
		t.Fatalf("backend sockets opened = %d, want a fresh one after the idle flow was dropped (2)", n)
	}
}

func TestRelayUDPCapsFlows(t *testing.T) {
	backend := echoWithPrefix(t, "")
	defer backend.Close()
	pub := listenUDP(t)
	dial := func() (*net.UDPConn, error) {
		return net.DialUDP("udp", nil, backend.LocalAddr().(*net.UDPAddr))
	}
	go relayUDP(pub, dial, time.Minute, 1)
	defer pub.Close()

	first, second := listenUDP(t), listenUDP(t)
	defer first.Close()
	defer second.Close()
	roundTrip(t, first, pub.LocalAddr(), "in")
	_, _ = second.WriteTo([]byte("over"), pub.LocalAddr())
	_ = second.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := second.Read(make([]byte, 16)); err == nil {
		t.Fatal("a client past the flow cap was relayed")
	}
}

func dnsQuery(id uint16, name string) []byte {
	q := make([]byte, 12)
	binary.BigEndian.PutUint16(q[0:], id)
	binary.BigEndian.PutUint16(q[2:], 0x0100)
	binary.BigEndian.PutUint16(q[4:], 1)
	for _, label := range []string{name} {
		q = append(q, byte(len(label)))
		q = append(q, label...)
	}
	q = append(q, 0, 0, 1, 0, 1)
	return q
}

func TestUDPIngressReachesSpaceNetns(t *testing.T) {
	skipUnlessRootlessNet(t)

	cn, err := NewSpaceNet("udp-ingress-it", "10.88.0.0/24")
	if err != nil {
		t.Fatalf("NewSpaceNet: %v", err)
	}
	defer cn.Close()
	gw := cn.Endpoints().Gateway
	if _, err := cn.send(command{Op: "register", Name: "resolver", IP: gw}); err != nil {
		t.Fatalf("register: %v", err)
	}

	pub := listenUDP(t)
	hostAddr := pub.LocalAddr()
	if err := cn.SendPacketConn("resolver", 53, pub); err != nil {
		t.Fatalf("SendPacketConn: %v", err)
	}

	client := listenUDP(t)
	defer client.Close()
	if _, err := client.WriteTo(dnsQuery(0xbeef, "resolver"), hostAddr); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 512)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatalf("no DNS reply through the udp relay: %v", err)
	}
	if n < 12 || binary.BigEndian.Uint16(buf) != 0xbeef || buf[2]&0x80 == 0 {
		t.Fatalf("reply is not a DNS response to our query: % x", buf[:n])
	}
}

func silentSink(t *testing.T) *net.UDPConn {
	t.Helper()
	sink := listenUDP(t)
	go func() {
		buf := make([]byte, 2048)
		for {
			if _, _, err := sink.ReadFromUDP(buf); err != nil {
				return
			}
		}
	}()
	return sink
}

func TestRelayUDPEvictsUnansweredFlowAtCap(t *testing.T) {
	sink := silentSink(t)
	defer sink.Close()
	backend := echoWithPrefix(t, "echo:")
	defer backend.Close()
	pub := listenUDP(t)
	var dials atomic.Int32
	dial := func() (*net.UDPConn, error) {
		to := backend
		if dials.Add(1) == 1 {
			to = sink
		}
		return net.DialUDP("udp", nil, to.LocalAddr().(*net.UDPAddr))
	}
	go relayUDP(pub, dial, time.Minute, 1)
	defer pub.Close()

	spoofed, real := listenUDP(t), listenUDP(t)
	defer spoofed.Close()
	defer real.Close()
	_, _ = spoofed.WriteTo([]byte("never answered"), pub.LocalAddr())
	time.Sleep(50 * time.Millisecond)
	if got := roundTrip(t, real, pub.LocalAddr(), "hi"); got != "echo:hi" {
		t.Fatalf("real client got %q", got)
	}
}

func TestRelayUDPCapsFlowsPerSourceIP(t *testing.T) {
	backend := echoWithPrefix(t, "")
	defer backend.Close()
	pub := listenUDP(t)
	var dials atomic.Int32
	dial := func() (*net.UDPConn, error) {
		dials.Add(1)
		return net.DialUDP("udp", nil, backend.LocalAddr().(*net.UDPAddr))
	}
	r := newUDPRelay(pub, dial, time.Minute, 16)
	r.maxPerIP = 2
	go r.serve()
	defer pub.Close()

	first, second, third := listenUDP(t), listenUDP(t), listenUDP(t)
	defer first.Close()
	defer second.Close()
	defer third.Close()
	roundTrip(t, first, pub.LocalAddr(), "1")
	roundTrip(t, second, pub.LocalAddr(), "2")
	roundTrip(t, third, pub.LocalAddr(), "3")
	if n := r.flowCount(); n != 2 {
		t.Fatalf("flows from one IP = %d, want the per-IP cap (2)", n)
	}
	roundTrip(t, first, pub.LocalAddr(), "1 again")
	if n := dials.Load(); n != 4 {
		t.Fatalf("backend sockets opened = %d, want the evicted oldest flow reopened (4)", n)
	}
}
