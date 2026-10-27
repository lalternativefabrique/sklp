//go:build linux

package spacenet

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"time"

	"github.com/vishvananda/netns"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// doPing runs inside the worker: it hops into the source peer's netns, sends
// an ICMP echo to dst, waits for the reply, then returns to the space netns.
// It proves L3 reachability across the bridge between two space netns. The
// worker holds CAP_NET_RAW (root in the space userns), so the unprivileged
// "udp4"/datagram ICMP socket is unnecessary — a raw "ip4:icmp" socket works.
func doPing(capNS netns.NsHandle, srcPID int, dst string) (err error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	srcNS, err := netns.GetFromPid(srcPID)
	if err != nil {
		return fmt.Errorf("open source netns (pid %d): %w", srcPID, err)
	}
	defer srcNS.Close()
	if err := netns.Set(srcNS); err != nil {
		return fmt.Errorf("enter source netns: %w", err)
	}
	defer func() {
		if setErr := netns.Set(capNS); setErr != nil && err == nil {
			err = fmt.Errorf("return to space netns: %w", setErr)
		}
	}()

	conn, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return fmt.Errorf("icmp socket in source netns: %w", err)
	}
	defer conn.Close()

	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Body: &icmp.Echo{ID: os.Getpid() & 0xffff, Seq: 1, Data: []byte("spacenet")},
	}
	wb, err := msg.Marshal(nil)
	if err != nil {
		return err
	}
	if _, err := conn.WriteTo(wb, &net.IPAddr{IP: net.ParseIP(dst)}); err != nil {
		return fmt.Errorf("send echo to %s: %w", dst, err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	rb := make([]byte, 1500)
	n, _, err := conn.ReadFrom(rb)
	if err != nil {
		return fmt.Errorf("no echo reply from %s: %w", dst, err)
	}
	rm, err := icmp.ParseMessage(1 /* iana.ProtocolICMP */, rb[:n])
	if err != nil {
		return fmt.Errorf("parse reply: %w", err)
	}
	if rm.Type != ipv4.ICMPTypeEchoReply {
		return fmt.Errorf("unexpected ICMP reply type %v from %s", rm.Type, dst)
	}
	return nil
}
