package netctrl

import (
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/lalternativefabrique/sklp/spacemanager/mesh"
)

// peerStaleAfter is how long a peer may go without registering or
// heartbeating before its services stop resolving here — 6x the
// Reconciler's 5s registration cadence, so a couple of missed ticks
// (transient authority hiccup, GC pause) never evict a live node, but a
// dead one stops receiving traffic within half a minute. Eviction is
// deliberately CLIENT-side: meshauthority stays a passive directory that
// never judges liveness, and wg0's peer set keeps the stale peer
// configured (tunnel plumbing is not service discovery — the node may
// come back).
const peerStaleAfter = 30 * time.Second

// PeerCache is a small, lock-protected, last-fetched snapshot of the mesh
// peer list, refreshed by the Reconciler on its own polling cadence. It
// never dials meshauthority itself — Refresh is spoon-fed by the caller, so
// netctrl's dependency on mesh stays a plain type import, no client of its
// own. A remote lookup miss here is not a failure: it just means no peer
// currently reports that service, same as a local Registry miss.
type PeerCache struct {
	mu     sync.RWMutex
	byKey  map[string]string // "<space>.<service>" -> remote peer's mesh IP
	byNode map[string]string // peer ID -> host of its WireGuard endpoint (the VM's private IP)
	selfID string            // this node's own meshauthority peer ID, skipped defensively
}

// NewPeerCache returns an empty cache. selfID is this node's own
// meshauthority peer ID (see mesh.Identity.NodeID) — Refresh skips that
// peer's own entries defensively; the local Registry always wins for a
// node's own services regardless (see dnsServer.handle), so this is cache
// hygiene, not a correctness requirement.
func NewPeerCache(selfID string) *PeerCache {
	return &PeerCache{byKey: map[string]string{}, byNode: map[string]string{}, selfID: selfID}
}

// Refresh replaces the cache wholesale from a freshly fetched peer list,
// skipping peers not seen by meshauthority within peerStaleAfter (a dead
// node's services must stop resolving, not blackhole traffic forever; a
// missing LastSeenAt is treated as fresh — never evict on absent data).
// A (space, service) published by more than one live peer resolves to
// nothing: picking one would hand the traffic to whichever peer lied, so the
// pair goes dark and is logged instead.
func (pc *PeerCache) Refresh(peers []mesh.Peer) {
	next := make(map[string]string, len(peers))
	nodes := make(map[string]string, len(peers))
	claimedBy := map[string]string{}
	contested := map[string][]string{}
	cutoff := time.Now().Add(-peerStaleAfter)
	for _, p := range peers {
		if p.ID == pc.selfID {
			continue
		}
		if p.LastSeenAt != nil && p.LastSeenAt.Before(cutoff) {
			continue
		}
		if host, _, err := net.SplitHostPort(p.Endpoint); err == nil {
			nodes[p.ID] = host
		}
		for _, s := range p.Services {
			k := key(s.Space, s.Service)
			if owner, taken := claimedBy[k]; taken && owner != p.ID {
				contested[k] = append(contested[k], p.ID)
				continue
			}
			claimedBy[k] = p.ID
			next[k] = p.PrivateIP
		}
	}
	for k, others := range contested {
		delete(next, k)
		fmt.Fprintf(os.Stderr, "netctrl: %s is published by peers %s and %s; not routing it until one withdraws\n", k, claimedBy[k], strings.Join(others, ", "))
	}
	pc.mu.Lock()
	pc.byKey = next
	pc.byNode = nodes
	pc.mu.Unlock()
}

// RemoteEndpoint returns the mesh IP of the peer hosting space/service, or
// ok=false if no known peer currently reports it.
func (pc *PeerCache) RemoteEndpoint(space, service string) (meshIP string, ok bool) {
	pc.mu.RLock()
	defer pc.mu.RUnlock()
	meshIP, ok = pc.byKey[key(space, service)]
	return meshIP, ok
}

// NodeHost returns the private-network address of the peer with the given
// mesh ID, the one the front relays that node's control API to.
func (pc *PeerCache) NodeHost(nodeID string) (host string, ok bool) {
	pc.mu.RLock()
	defer pc.mu.RUnlock()
	host, ok = pc.byNode[nodeID]
	return host, ok
}
