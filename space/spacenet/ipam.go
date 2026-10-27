//go:build linux

package spacenet

import (
	"fmt"
	"net"
	"sync"
)

// ipam is a trivial in-memory, thread-safe IPv4 allocator for one space
// subnet. It hands out host addresses sequentially starting just past the
// gateway (.1) and reclaims them on release. It is intentionally minimal:
// one space has a handful of containers, never a full /24 churn, so a
// linear scan over a free-list is cheaper than any bitmap bookkeeping.
type ipam struct {
	mu      sync.Mutex
	network *net.IPNet // the space subnet, e.g. 10.88.0.0/24
	gateway net.IP     // reserved bridge address, e.g. 10.88.0.1
	next    uint32     // next candidate host address as a uint32
	inUse   map[uint32]bool
}

// newIPAM builds an allocator for subnet (CIDR). The gateway is the first
// usable host address (network + 1) and is reserved up front, so Allocate
// never returns it.
func newIPAM(subnet string) (*ipam, error) {
	ip, ipnet, err := net.ParseCIDR(subnet)
	if err != nil {
		return nil, fmt.Errorf("spacenet: parse subnet %q: %w", subnet, err)
	}
	if ip.To4() == nil {
		return nil, fmt.Errorf("spacenet: subnet %q is not IPv4", subnet)
	}
	base := ipToU32(ipnet.IP)
	gw := base + 1 // .1 is the gateway by convention (matches the spike)
	return &ipam{
		network: ipnet,
		gateway: u32ToIP(gw),
		next:    gw + 1, // first container address is .2
		inUse:   map[uint32]bool{gw: true},
	}, nil
}

// gatewayCIDR returns the gateway address with the subnet mask, e.g.
// "10.88.0.1/24", ready for netlink.ParseAddr.
func (a *ipam) gatewayCIDR() string {
	ones, _ := a.network.Mask.Size()
	return fmt.Sprintf("%s/%d", a.gateway.String(), ones)
}

// allocate returns the next free host address as a "ip/prefix" CIDR string
// (so callers can hand it straight to netlink.ParseAddr) plus the bare IP.
// It skips the network, gateway, and broadcast addresses.
func (a *ipam) allocate() (cidr string, ip net.IP, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	ones, bits := a.network.Mask.Size()
	base := ipToU32(a.network.IP)
	broadcast := base | (0xffffffff >> uint(ones))
	hostMax := broadcast - 1 // last usable host address

	// Scan forward from next, wrapping once back to the first host address.
	first := base + 2 // .1 is gateway, so containers start at .2
	for span := 0; span <= int(hostMax-first); span++ {
		cand := a.next + uint32(span)
		if cand > hostMax {
			cand = first + (cand - hostMax - 1) // wrap into the host range
		}
		if cand < first || cand > hostMax || a.inUse[cand] {
			continue
		}
		a.inUse[cand] = true
		a.next = cand + 1
		return fmt.Sprintf("%s/%d", u32ToIP(cand).String(), ones), u32ToIP(cand), nil
	}
	_ = bits
	return "", nil, fmt.Errorf("spacenet: subnet %s exhausted", a.network.String())
}

// release returns an address to the pool. Unknown or reserved addresses are
// ignored so teardown is idempotent.
func (a *ipam) release(ip net.IP) {
	if ip == nil || ip.To4() == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.inUse, ipToU32(ip))
}

func ipToU32(ip net.IP) uint32 {
	ip = ip.To4()
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

func u32ToIP(v uint32) net.IP {
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v)).To4()
}
