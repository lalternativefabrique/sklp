package control

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
)

// RestoreSource reports the node's restore targets with their drill history.
// Like the inventory, it answers for the machine, not for one space.
type RestoreSource interface {
	RestoresJSON(ctx context.Context) (json.RawMessage, error)
}

type RestoreSourceFunc func(ctx context.Context) (json.RawMessage, error)

func (f RestoreSourceFunc) RestoresJSON(ctx context.Context) (json.RawMessage, error) {
	return f(ctx)
}

var (
	restoresMu sync.RWMutex
	restores   RestoreSource
)

func SetRestores(src RestoreSource) {
	restoresMu.Lock()
	defer restoresMu.Unlock()
	restores = src
}

func currentRestores() RestoreSource {
	restoresMu.RLock()
	defer restoresMu.RUnlock()
	return restores
}

func serveRestores(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "restores: GET required", http.StatusMethodNotAllowed)
		return
	}
	src := currentRestores()
	if src == nil {
		http.Error(w, "restores: this node runs no restore drills", http.StatusNotImplemented)
		return
	}
	targets, err := src.RestoresJSON(r.Context())
	if err != nil {
		http.Error(w, "restores: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, RestoresResponse{Node: NodeName(), Targets: targets})
}
