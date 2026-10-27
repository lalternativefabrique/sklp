package mesh

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// lastKnownState is what a node needs to rebuild wg0 without meshauthority:
// its own assigned mesh IP and the peer list it last applied.
type lastKnownState struct {
	V          int    `json:"v"`
	SelfMeshIP string `json:"self_mesh_ip"`
	Peers      []Peer `json:"peers"`
}

func lastKnownPath() string { return filepath.Join(meshConfigDir(), "peers.json") }

// saveLastKnown records the peer list a node just applied, next to the
// identity it belongs to. Best effort: failing to cache must never fail a
// join that otherwise succeeded, so callers log rather than propagate.
func saveLastKnown(selfMeshIP string, peers []Peer) error {
	b, err := json.MarshalIndent(lastKnownState{V: 1, SelfMeshIP: selfMeshIP, Peers: peers}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode peer cache: %w", err)
	}
	if err := os.MkdirAll(meshConfigDir(), 0o700); err != nil {
		return fmt.Errorf("create mesh config dir: %w", err)
	}
	return atomicWrite(lastKnownPath(), b, 0o600)
}

// loadLastKnown returns the last applied peer list, or ok=false when there is
// none to fall back on.
func loadLastKnown() (state lastKnownState, ok bool) {
	b, err := os.ReadFile(lastKnownPath())
	if err != nil {
		return lastKnownState{}, false
	}
	if err := json.Unmarshal(b, &state); err != nil {
		return lastKnownState{}, false
	}
	if state.SelfMeshIP == "" {
		return lastKnownState{}, false
	}
	return state, true
}

// RememberPeers persists a successful registration so the next boot's Join
// can bring wg0 up from it when meshauthority is unreachable at that moment,
// which is always the case on the node that hosts meshauthority itself: the
// authority is a Space, launched after the join.
func RememberPeers(selfMeshIP string, peers []Peer) error {
	return saveLastKnown(selfMeshIP, peers)
}
