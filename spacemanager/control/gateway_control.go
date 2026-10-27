package control

import (
	"encoding/json"
	"net/http"
	"sync"
)

// GatewayControlStatus is the wire shape of RouteGateway's answer — a copy of
// spacemanager.GatewayStatus kept here so the control package (and its
// clients) never import gateway code.
type GatewayControlStatus struct {
	Driver    string `json:"driver"`
	Running   bool   `json:"running"`
	Listening string `json:"listening"`
}

// gatewayControl is the installed hook the node's gateway owner (the
// controller's gatewayManager) registers, mirroring installInventory: the
// control package serves the routes, the owner supplies the behaviour. Nil
// until a gateway is opened — the handlers answer 503 then, which is the
// honest state ("this node has no gateway to pilot"), not a 404.
var gatewayControl struct {
	mu     sync.Mutex
	status func() (GatewayControlStatus, error)
	up     func() error
}

// InstallGatewayControl registers the node's gateway status/up hooks. Last
// install wins (one gateway singleton per host; a re-acquire re-installs the
// same driver's hooks).
func InstallGatewayControl(status func() (GatewayControlStatus, error), up func() error) {
	gatewayControl.mu.Lock()
	defer gatewayControl.mu.Unlock()
	gatewayControl.status = status
	gatewayControl.up = up
}

func gatewayHooks() (status func() (GatewayControlStatus, error), up func() error) {
	gatewayControl.mu.Lock()
	defer gatewayControl.mu.Unlock()
	return gatewayControl.status, gatewayControl.up
}

// serveGatewayStatus answers GET RouteGateway.
func serveGatewayStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	status, _ := gatewayHooks()
	if status == nil {
		http.Error(w, "no gateway is opened on this node", http.StatusServiceUnavailable)
		return
	}
	st, err := status()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}

// serveGatewayUp answers POST RouteGatewayUp: idempotent EnsureUp, then the
// resulting status so the caller sees what it got in one round-trip.
func serveGatewayUp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	status, up := gatewayHooks()
	if up == nil {
		http.Error(w, "no gateway is opened on this node", http.StatusServiceUnavailable)
		return
	}
	if err := up(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var st GatewayControlStatus
	if status != nil {
		st, _ = status()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}
