//go:build linux

package spacenet

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"strings"
	"sync"
)

// bridgeMAC returns a deterministic, locally-administered unicast MAC derived
// from the bridge name. The first octet is 0x02 (locally administered, unicast)
// so it never collides with a real NIC; the rest hashes the name so each
// space bridge has a stable, distinct address that survives port churn.
func bridgeMAC(name string) net.HardwareAddr {
	sum := sha1.Sum([]byte(name))
	return net.HardwareAddr{0x02, sum[0], sum[1], sum[2], sum[3], sum[4]}
}

// nameTable maps a service name to every IP currently registered under it.
// A service with N Ready replicas registers N IPs under one name; A queries
// rotate over them (round-robin DNS), so traffic spreads across replicas
// without any external load balancer. It is consulted by the DNS resolver on
// every query, so it is safe for concurrent register/unregister/lookup.
//
// Registration follows the network attachment; routing follows the deploy
// agent. Once the agent has pinned a routed set for a name, lookup answers only
// the registered IPs in that set, so an instance that is attached but not Ready,
// or retired by a rollout cutover, receives no new traffic while it stays
// reachable for the east-west ACL (members). A name never pinned answers every
// registered IP.
type nameTable struct {
	mu     sync.Mutex
	ips    map[string][]net.IP // service name -> registered IPs (insertion order)
	rr     map[string]int      // per-name round-robin cursor
	routed map[string][]net.IP // service name -> IPs the agent routes traffic to
}

func newNameTable() *nameTable {
	return &nameTable{ips: map[string][]net.IP{}, rr: map[string]int{}, routed: map[string][]net.IP{}}
}

// route pins the set of IPs that receive traffic for name. An empty set is a
// valid pin: the name resolves to nothing until an instance is routed.
func (t *nameTable) route(name string, ips []net.IP) {
	name = strings.ToLower(name)
	if name == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.routed[name] = append([]net.IP{}, ips...)
}

// members returns every IP registered under name, routed or not, in insertion
// order and without advancing the round-robin cursor.
func (t *nameTable) members(name string) []net.IP {
	name = strings.ToLower(name)
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]net.IP(nil), t.ips[name]...)
}

func (t *nameTable) servingLocked(name string) (ips []net.IP, pinned bool) {
	cur := t.ips[name]
	routed, pinned := t.routed[name]
	if !pinned {
		return cur, false
	}
	out := make([]net.IP, 0, len(routed))
	for _, ip := range cur {
		for _, r := range routed {
			if ip.Equal(r) {
				out = append(out, ip)
				break
			}
		}
	}
	return out, true
}

// register adds ip under name (deduplicated). The name is matched
// case-insensitively at lookup time, so it is stored lowercased here.
func (t *nameTable) register(name string, ip net.IP) {
	name = strings.ToLower(name)
	if name == "" || ip == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, have := range t.ips[name] {
		if have.Equal(ip) {
			return
		}
	}
	t.ips[name] = append(t.ips[name], ip)
}

// unregister drops ip from name; the name entry is removed once empty so a
// query for a fully-released service NXDOMAINs rather than returning stale IPs.
func (t *nameTable) unregister(name string, ip net.IP) {
	name = strings.ToLower(name)
	t.mu.Lock()
	defer t.mu.Unlock()
	cur := t.ips[name]
	for i, have := range cur {
		if have.Equal(ip) {
			cur = append(cur[:i], cur[i+1:]...)
			break
		}
	}
	if len(cur) == 0 {
		delete(t.ips, name)
		delete(t.rr, name)
		return
	}
	t.ips[name] = cur
}

// lookup returns the IPs serving name, rotated by one each call so the
// first answer round-robins across replicas. The full set is still returned
// (rotated) so a client that ignores order, or caches all A records, also
// balances. ok is false when the name is neither registered nor pinned (caller
// should forward it); a pinned name with nothing routed is ok with no IPs.
func (t *nameTable) lookup(name string) (ips []net.IP, ok bool) {
	name = strings.ToLower(name)
	t.mu.Lock()
	defer t.mu.Unlock()
	cur, pinned := t.servingLocked(name)
	if len(cur) == 0 {
		return nil, pinned
	}
	off := t.rr[name] % len(cur)
	t.rr[name] = (t.rr[name] + 1) % len(cur)
	out := make([]net.IP, 0, len(cur))
	out = append(out, cur[off:]...)
	out = append(out, cur[:off]...)
	return out, true
}

// Linux interface names are capped at 15 bytes (IFNAMSIZ-1). Bridge and veth
// names are derived from the space name, so we use a short HASH fragment of
// the FULL name (not a truncation of it) to leave room for the fixed
// prefixes/suffixes WITHOUT collisions: truncating two long names that share a
// prefix (web-staging-1 / web-staging-2) to 7 chars would map them to the same
// bridge / veth / socket, silently bridging two spaces. nameFrag avoids that.

// bridgeName is "cap-<frag>-br", e.g. cap-7d3a9f1-br, always <= 15 bytes.
// The 7-hex fragment of the full name is collision-resistant across the handful
// of spaces a host runs, unlike a 7-char prefix truncation.
func bridgeName(name string) string {
	return "cap-" + nameFrag(name, 7) + "-br"
}

// nameFrag returns the first n hex chars of the SHA-256 of the full name — a
// stable, collision-resistant fragment for interface names and per-space socket
// paths. Distinct names give distinct fragments (no shared-prefix truncation
// collision); the same name always gives the same fragment (stable across runs).
func nameFrag(name string, n int) string {
	sum := sha256.Sum256([]byte(name))
	h := hex.EncodeToString(sum[:])
	if n > len(h) {
		n = len(h)
	}
	return h[:n]
}

// short keeps only [a-z0-9] from the space name, lowercased, for use as a
// human-readable file-name fragment (resolv.conf, hosts) where CreateTemp adds
// its own uniqueness suffix, so truncation/collision there is harmless. Empty
// input collapses to "x". For collision-sensitive names use nameFrag.
func short(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "x"
	}
	return b.String()
}
