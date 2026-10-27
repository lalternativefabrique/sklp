// Package netctrl is the Network Controller: a single process, external to
// every Space, that owns all cross-Space traffic. No Space ever dials
// another Space's real endpoint IP directly — every cross-Space call, HTTP
// or raw TCP, is mediated here. A service becomes reachable purely by
// declaring port: in its own manifest; there is no separate allow-list.
package netctrl

import (
	"strings"
	"sync"
)

// Endpoint is one service's real backend address, as seen by the controller.
type Endpoint struct {
	IP   string
	Port int
}

// Registry is the controller's in-memory source of truth: which service, in
// which Space, is currently reachable and where. v1 is a plain map — no
// NATS/JetStream, no cross-node federation, since the controller itself is
// mono-node for now. Multi-node propagation is a deliberate v2 problem.
type Registry struct {
	mu       sync.RWMutex
	services map[string]Endpoint // key: "<space>.<service>"
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{services: map[string]Endpoint{}}
}

// key is lowercased because DNS names are case-insensitive (parseInternalName
// lowercases every query it parses) and HTTP Host headers vary in case across
// clients — the Registry must match either regardless of how Put registered
// the service.
func key(space, service string) string {
	return strings.ToLower(space) + "." + strings.ToLower(service)
}

// Put registers (or updates) a service's endpoint. Called whenever a Space
// attaches a service with a declared port:, or an existing endpoint's
// address changes.
func (r *Registry) Put(space, service string, ep Endpoint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.services[key(space, service)] = ep
}

// Delete removes one service. Called on detach or when a healthcheck marks
// a service permanently down.
func (r *Registry) Delete(space, service string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.services, key(space, service))
}

// DeleteAll removes every service belonging to a Space. Called on full Space
// teardown, so a destroyed Space leaves no stale entries behind.
func (r *Registry) DeleteAll(space string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prefix := strings.ToLower(space) + "."
	for k := range r.services {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			delete(r.services, k)
		}
	}
}

// Lookup returns the endpoint for space/service, or ok=false if it is not
// registered (either it never declared port:, or it has since been removed).
func (r *Registry) Lookup(space, service string) (ep Endpoint, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ep, ok = r.services[key(space, service)]
	return ep, ok
}
