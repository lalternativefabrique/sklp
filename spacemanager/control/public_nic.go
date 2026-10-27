package control

import (
	"encoding/json"
	"net/http"
	"sync"
)

var (
	publicNICMu     sync.RWMutex
	publicNICStatus func() (json.RawMessage, error)
)

// SetPublicNIC installs the node's public NIC reconciler status; nil leaves
// the route answering 501 on a node without a public_nic block.
func SetPublicNIC(status func() (json.RawMessage, error)) {
	publicNICMu.Lock()
	defer publicNICMu.Unlock()
	publicNICStatus = status
}

func servePublicNIC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "public nic: GET required", http.StatusMethodNotAllowed)
		return
	}
	publicNICMu.RLock()
	status := publicNICStatus
	publicNICMu.RUnlock()
	if status == nil {
		http.Error(w, "public nic: this node manages no public NIC", http.StatusNotImplemented)
		return
	}
	body, err := status()
	if err != nil {
		http.Error(w, "public nic: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, PublicNICResponse{Node: NodeName(), Status: body})
}
