package mesh

import (
	"context"
	"fmt"
	"os"
)

// Join brings this node's mesh identity, meshauthority registration, and
// local wg0 interface all in sync, in one call: the sequence a node runs
// once per boot (ADR-006). authorityURL/token select the meshauthority
// instance; endpoint is this node's own dialable IP:port (private cloud
// interface address + the WireGuard listen port) so other peers can
// initiate a handshake to it. spaces is this node's current local Space
// list (ADR-008) — the source the cross-host relay resolves "which node
// runs Space X" from. services is this node's current local service list
// (the Network Controller's cross-node extension) — nil at boot time is
// fine, since the Network Controller isn't up yet; it re-registers with
// the real list on its own polling cadence once it is. Returns this node's
// own assigned mesh IP so the caller can bring up whatever else needs to
// bind wg0 (ADR-008's cross-host relay receiver, the Network Controller's
// mesh-bound listeners).
func Join(ctx context.Context, authorityURL, token, certFingerprint, endpoint string, spaces []string, services []ServiceRef) (selfMeshIP string, err error) {
	id, err := EnsureIdentity()
	if err != nil {
		return "", fmt.Errorf("mesh identity: %w", err)
	}

	client := NewPinnedClient(authorityURL, token, id.SigningKey, certFingerprint)
	self, peers, err := client.Register(ctx, id.NodeID, id.PrivateKey.PublicKey().String(), endpoint, spaces, services)
	if err != nil {
		// meshauthority is the control plane, never the data path: a mesh
		// already up keeps carrying traffic while it is down. A node REBOOTING
		// during that window used to be the exception — it returned here before
		// ever creating wg0, so it stayed out of the mesh entirely until the
		// authority came back. Fall back to the peer list this node last
		// applied instead: keys and mesh IPs are stable by construction (the
		// identity is persisted, the IP allocation lives in the registry), so
		// yesterday's list still describes today's mesh.
		state, ok := loadLastKnown()
		if !ok {
			return "", fmt.Errorf("register with meshauthority: %w", err)
		}
		if ierr := EnsureInterface(state.SelfMeshIP + "/24"); ierr != nil {
			return "", fmt.Errorf("register with meshauthority: %w (and ensure %s from cache: %v)", err, InterfaceName, ierr)
		}
		if serr := SyncPeers(id.PrivateKey, id.NodeID, state.Peers); serr != nil {
			return "", fmt.Errorf("register with meshauthority: %w (and sync cached peers: %v)", err, serr)
		}
		// Degraded on purpose: a peer whose key changed while the authority was
		// unreachable stays unreachable until the next successful Register,
		// which the Reconciler retries on its own cadence.
		return state.SelfMeshIP, fmt.Errorf("meshauthority unreachable, brought %s up from the last known peer list: %w", InterfaceName, err)
	}

	if err := EnsureInterface(self.PrivateIP + "/24"); err != nil {
		return "", fmt.Errorf("ensure %s: %w", InterfaceName, err)
	}
	if err := SyncPeers(id.PrivateKey, id.NodeID, peers); err != nil {
		return "", fmt.Errorf("sync peers: %w", err)
	}
	if serr := saveLastKnown(self.PrivateIP, peers); serr != nil {
		// Not fatal: the mesh is up, only the fallback for the next boot is not.
		fmt.Fprintf(os.Stderr, "mesh: cache peer list: %v\n", serr)
	}
	return self.PrivateIP, nil
}
