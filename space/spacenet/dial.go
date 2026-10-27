//go:build linux

package spacenet

import (
	"fmt"
	"net"
	"runtime"
	"time"

	"github.com/vishvananda/netns"
)

// Dial opens a TCP connection from the netns of the peer whose init pid is
// srcPID to addr ("host:port"), proving real egress: the packet leaves over the
// container's default route -> bridge gateway -> (forwarded) -> tap0 -> NAT ->
// the host's stack -> the Internet. It returns nil iff the connect succeeds. It
// is only meaningful once EnableEgress has run; tests gate it behind a
// real-network skip since CI may have no outbound path.
func (c *SpaceNet) Dial(srcPID int, addr string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("spacenet: %q is closed", c.name)
	}
	rep, err := c.send(command{Op: "dial", PID: srcPID, Dst: addr})
	if err != nil {
		return err
	}
	if !rep.OK {
		return fmt.Errorf("spacenet: dial %s from pid %d: %s", addr, srcPID, rep.Error)
	}
	return nil
}

// doDial runs inside the worker: it hops into the peer's netns, dials addr over
// TCP, then returns to the space netns. Mirrors doPing's netns discipline.
func doDial(capNS netns.NsHandle, srcPID int, addr string) (err error) {
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

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	_ = conn.Close()
	return nil
}
