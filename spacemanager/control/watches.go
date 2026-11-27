//go:build linux

package control

import (
	"context"
	"net/http"
	"sync"
)

type WatchSource interface {
	ListWatches(ctx context.Context) ([]WatchEntry, error)
}

var (
	watchesMu sync.RWMutex
	watches   WatchSource
)

func SetWatches(src WatchSource) {
	watchesMu.Lock()
	defer watchesMu.Unlock()
	watches = src
}

func serveWatches(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "watches: GET required", http.StatusMethodNotAllowed)
		return
	}
	watchesMu.RLock()
	src := watches
	watchesMu.RUnlock()
	list := []WatchEntry{}
	if src != nil {
		got, err := src.ListWatches(r.Context())
		if err != nil {
			http.Error(w, "watches: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if got != nil {
			list = got
		}
	}
	writeJSON(w, WatchesResponse{Node: NodeName(), Watches: list})
}
