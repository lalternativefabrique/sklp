//go:build linux

package spacenet

import (
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"sync"
	"time"

	"github.com/vishvananda/netns"
)

const (
	udpFlowIdle       = 2 * time.Minute
	udpUnansweredIdle = 10 * time.Second
	udpMaxFlows       = 4096
	udpMaxFlowsPerIP  = 64
	udpMaxDatagram    = 65535
	udpReplyBuffer    = 8192
)

type udpDialer func() (*net.UDPConn, error)

// udpFlow is one client's path into the space: its own backend socket, so the
// replies the service sends back are relayed to that client alone. A flow the
// service never answered may come from a spoofed source address, so it is the
// first evicted and expires sooner.
type udpFlow struct {
	backend  *net.UDPConn
	mu       sync.Mutex
	seen     time.Time
	answered bool
}

func (f *udpFlow) touch(now time.Time, answered bool) {
	f.mu.Lock()
	f.seen = now
	f.answered = f.answered || answered
	f.mu.Unlock()
}

func (f *udpFlow) state(now time.Time) (idle time.Duration, answered bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return now.Sub(f.seen), f.answered
}

type udpRelay struct {
	pub            *net.UDPConn
	dial           udpDialer
	idle           time.Duration
	unansweredIdle time.Duration
	maxFlows       int
	maxPerIP       int

	mu    sync.Mutex
	flows map[netip.AddrPort]*udpFlow
	perIP map[netip.Addr]int
}

// relayUDP serves pub, a socket bound in the host netns, until it is closed:
// each client address gets a backend socket from dial, and datagrams are
// copied both ways. A flow silent for idle is dropped. At maxFlows a new client
// takes the place of the oldest unanswered flow, or is ignored when every flow
// has been answered, so spoofed sources cannot crowd out real clients. One
// source IP holds at most udpMaxFlowsPerIP flows: past it, its own oldest flow
// gives way, so one host cycling source ports cannot fill the table.
func relayUDP(pub *net.UDPConn, dial udpDialer, idle time.Duration, maxFlows int) {
	newUDPRelay(pub, dial, idle, maxFlows).serve()
}

func newUDPRelay(pub *net.UDPConn, dial udpDialer, idle time.Duration, maxFlows int) *udpRelay {
	return &udpRelay{
		pub: pub, dial: dial, idle: idle, unansweredIdle: min(udpUnansweredIdle, idle),
		maxFlows: maxFlows, maxPerIP: udpMaxFlowsPerIP,
		flows: map[netip.AddrPort]*udpFlow{}, perIP: map[netip.Addr]int{},
	}
}

func (r *udpRelay) flowCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.flows)
}

func (r *udpRelay) serve() {
	pub := r.pub
	stop := make(chan struct{})
	defer close(stop)
	go r.reapIdle(stop)
	defer r.closeAll()

	buf := make([]byte, udpMaxDatagram)
	for {
		n, client, err := pub.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		f := r.flowFor(client, func() (*udpFlow, error) {
			backend, err := r.dial()
			if err != nil {
				return nil, err
			}
			f := &udpFlow{backend: backend, seen: time.Now()}
			go r.replies(f, net.UDPAddrFromAddrPort(client))
			return f, nil
		})
		if f == nil {
			continue
		}
		f.touch(time.Now(), false)
		_, _ = f.backend.Write(buf[:n])
	}
}

func (r *udpRelay) flowFor(key netip.AddrPort, open func() (*udpFlow, error)) *udpFlow {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.flows[key]; ok {
		return f
	}
	if r.perIP[key.Addr()] >= r.maxPerIP {
		r.evictOldestOfLocked(key.Addr())
	}
	if len(r.flows) >= r.maxFlows && !r.evictUnansweredLocked() {
		return nil
	}
	f, err := open()
	if err != nil {
		return nil
	}
	r.flows[key] = f
	r.perIP[key.Addr()]++
	return f
}

func (r *udpRelay) dropLocked(key netip.AddrPort) {
	f, ok := r.flows[key]
	if !ok {
		return
	}
	_ = f.backend.Close()
	delete(r.flows, key)
	if r.perIP[key.Addr()]--; r.perIP[key.Addr()] <= 0 {
		delete(r.perIP, key.Addr())
	}
}

func (r *udpRelay) replies(f *udpFlow, client *net.UDPAddr) {
	buf := make([]byte, udpReplyBuffer)
	for {
		n, err := f.backend.Read(buf)
		if err != nil {
			return
		}
		f.touch(time.Now(), true)
		_, _ = r.pub.WriteToUDP(buf[:n], client)
	}
}

func (r *udpRelay) evictOldestOfLocked(ip netip.Addr) {
	now := time.Now()
	var oldest netip.AddrPort
	var oldestIdle time.Duration = -1
	for k, f := range r.flows {
		if k.Addr() != ip {
			continue
		}
		if idle, _ := f.state(now); idle > oldestIdle {
			oldest, oldestIdle = k, idle
		}
	}
	if oldestIdle >= 0 {
		r.dropLocked(oldest)
	}
}

func (r *udpRelay) evictUnansweredLocked() bool {
	now := time.Now()
	var oldest netip.AddrPort
	var oldestIdle time.Duration = -1
	for k, f := range r.flows {
		idle, answered := f.state(now)
		if !answered && idle > oldestIdle {
			oldest, oldestIdle = k, idle
		}
	}
	if oldestIdle < 0 {
		return false
	}
	r.dropLocked(oldest)
	return true
}

func (r *udpRelay) reapIdle(stop <-chan struct{}) {
	t := time.NewTicker(r.unansweredIdle / 4)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			r.mu.Lock()
			for k, f := range r.flows {
				idle, answered := f.state(now)
				if idle >= r.idle || (!answered && idle >= r.unansweredIdle) {
					r.dropLocked(k)
				}
			}
			r.mu.Unlock()
		}
	}
}

func (r *udpRelay) closeAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k := range r.flows {
		r.dropLocked(k)
	}
}

// netnsUDPDialer returns a dialer whose sockets are created in capNS. The
// sockets are created on one OS thread pinned to the netns for the relay's
// life; a socket keeps its netns once created, so the relay uses it anywhere.
func netnsUDPDialer(capNS netns.NsHandle, names *nameTable, service string, port int) (udpDialer, func()) {
	type result struct {
		conn *net.UDPConn
		err  error
	}
	reqs := make(chan chan result)
	done := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		if err := netns.Set(capNS); err != nil {
			runtime.UnlockOSThread()
			for {
				select {
				case <-done:
					return
				case reply := <-reqs:
					reply <- result{err: fmt.Errorf("udp relay: enter space netns: %w", err)}
				}
			}
		}
		for {
			select {
			case <-done:
				return
			case reply := <-reqs:
				ips, ok := names.lookup(service)
				if !ok || len(ips) == 0 {
					reply <- result{err: fmt.Errorf("udp relay: service %q has no ready replica", service)}
					continue
				}
				conn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: ips[0], Port: port})
				reply <- result{conn: conn, err: err}
			}
		}
	}()
	dial := func() (*net.UDPConn, error) {
		reply := make(chan result, 1)
		select {
		case reqs <- reply:
		case <-done:
			return nil, fmt.Errorf("udp relay: stopped")
		}
		res := <-reply
		return res.conn, res.err
	}
	return dial, func() { close(done) }
}
