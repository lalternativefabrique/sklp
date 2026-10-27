package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
)

// ErrMeshPeerNotFound is what a MeshDirectory returns for an unknown peer id.
var ErrMeshPeerNotFound = errors.New("mesh peer not found")

// MeshDirectory is the node's view of meshauthority, installed by the agent
// when it joined a mesh. Nil until then: the handlers answer 503, which is
// the honest state ("this node is not on a mesh"), not a 404.
type MeshDirectory interface {
	ListPeers(ctx context.Context) ([]MeshPeer, error)
	DeletePeer(ctx context.Context, id string) error
}

var meshDirectory struct {
	mu  sync.Mutex
	dir MeshDirectory
}

// InstallMeshDirectory registers the node's meshauthority client. Last
// install wins.
func InstallMeshDirectory(dir MeshDirectory) {
	meshDirectory.mu.Lock()
	defer meshDirectory.mu.Unlock()
	meshDirectory.dir = dir
}

func meshDir() MeshDirectory {
	meshDirectory.mu.Lock()
	defer meshDirectory.mu.Unlock()
	return meshDirectory.dir
}

// serveMeshPeers answers GET RouteMeshPeers and DELETE RouteMeshPeers/<id>.
func serveMeshPeers(w http.ResponseWriter, r *http.Request) {
	dir := meshDir()
	if dir == nil {
		http.Error(w, "this node is not on a mesh", http.StatusServiceUnavailable)
		return
	}
	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, RouteMeshPeers), "/")
	switch {
	case r.Method == http.MethodGet && id == "":
		peers, err := dir.ListPeers(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MeshPeersResponse{Peers: peers})
	case r.Method == http.MethodDelete && id != "":
		if err := dir.DeletePeer(r.Context(), id); err != nil {
			if errors.Is(err, ErrMeshPeerNotFound) {
				http.Error(w, "mesh peer "+id+" not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
