//go:build linux

package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
)

var (
	ErrFollowInvalid  = errors.New("invalid follow")
	ErrFollowConflict = errors.New("follow conflict")
	ErrFollowMissing  = errors.New("follow not found")
)

// FollowSource is the node's list of followed repos. Errors wrap one of the
// ErrFollow* values so the route answers the matching status.
type FollowSource interface {
	ListFollows(ctx context.Context) (any, error)
	AddFollow(ctx context.Context, req FollowRequest) (any, error)
	RemoveFollow(ctx context.Context, name string) error
}

var (
	followsMu sync.RWMutex
	follows   FollowSource
)

func SetFollows(src FollowSource) {
	followsMu.Lock()
	defer followsMu.Unlock()
	follows = src
}

func currentFollows() FollowSource {
	followsMu.RLock()
	defer followsMu.RUnlock()
	return follows
}

func serveFollows(w http.ResponseWriter, r *http.Request) {
	src := currentFollows()
	if src == nil {
		http.Error(w, "follows: this node does not reconcile from repos", http.StatusNotImplemented)
		return
	}
	name := strings.Trim(strings.TrimPrefix(r.URL.Path, RouteFollows), "/")
	switch {
	case r.Method == http.MethodGet && name == "":
		list, err := src.ListFollows(r.Context())
		if err != nil {
			followError(w, err)
			return
		}
		writeJSON(w, map[string]any{"node": NodeName(), "follows": list})
	case r.Method == http.MethodPost && name == "":
		var req FollowRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "follows: decode request: "+err.Error(), http.StatusBadRequest)
			return
		}
		added, err := src.AddFollow(r.Context(), req)
		if err != nil {
			followError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(added)
	case r.Method == http.MethodDelete && name != "":
		if err := src.RemoveFollow(r.Context(), name); err != nil {
			followError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "follows: GET or POST "+RouteFollows+", DELETE "+RouteFollows+"/<name>", http.StatusMethodNotAllowed)
	}
}

func followError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrFollowInvalid):
		http.Error(w, "follows: "+err.Error(), http.StatusBadRequest)
	case errors.Is(err, ErrFollowConflict):
		http.Error(w, "follows: "+err.Error(), http.StatusConflict)
	case errors.Is(err, ErrFollowMissing):
		http.Error(w, "follows: "+err.Error(), http.StatusNotFound)
	default:
		http.Error(w, "follows: "+err.Error(), http.StatusInternalServerError)
	}
}
