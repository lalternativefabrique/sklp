//go:build linux

package spacenet

import (
	"fmt"
	"net"
	"runtime"
	"time"

	"github.com/miekg/dns"
	"github.com/vishvananda/netns"
)

// doResolve runs inside the worker: it hops into the source peer's netns, asks
// the space resolver (server, "ip:53") to resolve an A record for name, then
// returns to the space netns. It returns the answer IPs as strings. This
// proves a container reaches the in-space DNS over the bridge and gets the
// right service IP — the step-2 go/no-go, without needing dig/nslookup in the
// peer. It mirrors doPing's netns-hop discipline.
func doResolve(capNS netns.NsHandle, srcPID int, server, name string) (ips []string, err error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	srcNS, err := netns.GetFromPid(srcPID)
	if err != nil {
		return nil, fmt.Errorf("open source netns (pid %d): %w", srcPID, err)
	}
	defer srcNS.Close()
	if err := netns.Set(srcNS); err != nil {
		return nil, fmt.Errorf("enter source netns: %w", err)
	}
	defer func() {
		if setErr := netns.Set(capNS); setErr != nil && err == nil {
			err = fmt.Errorf("return to space netns: %w", setErr)
		}
	}()

	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	c := &dns.Client{Timeout: time.Second}

	// Retry like a real resolv.conf client (glibc/musl retry on timeout): a
	// single UDP datagram can be dropped during bridge port churn, and a libc
	// resolver would not give up after one lost packet. Three short attempts.
	var resp *dns.Msg
	for attempt := 0; attempt < 3; attempt++ {
		resp, _, err = c.Exchange(m, server)
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("dns query %q to %s: %w", name, server, err)
	}
	if resp.Rcode != dns.RcodeSuccess {
		return nil, fmt.Errorf("dns query %q: rcode %s", name, dns.RcodeToString[resp.Rcode])
	}
	for _, rr := range resp.Answer {
		if a, ok := rr.(*dns.A); ok {
			ips = append(ips, net.IP(a.A).String())
		}
	}
	return ips, nil
}
