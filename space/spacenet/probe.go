//go:build linux

package spacenet

import (
	"context"
	"net"
	"net/http"
	"runtime"
	"time"

	"github.com/vishvananda/netns"
)

// probeTimeout caps a single readiness dial/GET. Short: a probe is polled, so a
// hung connect must not stall the poll cadence.
const probeTimeout = 2 * time.Second

// ProbeTCP reports whether a TCP connection to addr ("ip:port") succeeds FROM
// the space netns. It is the reliable readiness primitive for deploy: the
// space bridge netns reaches every attached container IP directly, so a probe
// dials the container's bridge IP natively (Go net.Dial) instead of depending
// on a `runc exec nc` binary that may not exist in the image. Returns false on
// any failure (refused, no route, timeout); a probe is best-effort by nature.
func (c *SpaceNet) ProbeTCP(addr string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	rep, err := c.send(command{Op: "probeTCP", Dst: addr})
	if err != nil {
		return false
	}
	return rep.OK
}

// ProbeHTTP reports whether a GET on url returns a 2xx status FROM the space
// netns. Like ProbeTCP, it dials the container's bridge IP natively rather than
// exec'ing wget inside the container. Returns false on any transport error or a
// non-2xx status.
func (c *SpaceNet) ProbeHTTP(url string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	rep, err := c.send(command{Op: "probeHTTP", Dst: url})
	if err != nil {
		return false
	}
	return rep.OK
}

// doProbeTCP runs inside the worker on the space netns: it dials addr over
// TCP and reports success. The worker's main thread is already pinned to the
// space netns; we lock the OS thread and re-assert capNS so the connect
// syscall cannot be scheduled onto a host-netns thread.
func doProbeTCP(capNS netns.NsHandle, addr string) bool {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := netns.Set(capNS); err != nil {
		return false
	}
	conn, err := net.DialTimeout("tcp", addr, probeTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// doProbeHTTP runs inside the worker on the space netns: it GETs url through a
// client whose dialer is pinned to the space netns thread, and reports a 2xx.
func doProbeHTTP(capNS netns.NsHandle, url string) bool {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := netns.Set(capNS); err != nil {
		return false
	}
	dialer := &net.Dialer{Timeout: probeTimeout}
	client := &http.Client{
		Timeout: probeTimeout,
		Transport: &http.Transport{
			// DialContext runs on this same OS-thread-locked goroutine, so the
			// connect happens in the space netns; disable keep-alives so no
			// pooled connection survives onto another netns thread.
			DialContext:       dialer.DialContext,
			DisableKeepAlives: true,
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
