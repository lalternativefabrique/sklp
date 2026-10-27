package control

import "sync"

// spaceHandle is what the multi-space dispatcher needs to serve one space's
// routes: its Supervisor plus the shutdown hook POST /v1/down calls.
type spaceHandle struct {
	sup Supervisor
	// shutdown STOPS the space, keeping its manifest so it can be started
	// again; purgeShutdown deletes it, and deleteShutdown deletes it with its
	// named volumes. POST /v1/down picks one per its DownRequest.Purge /
	// .Volumes — see the Down route.
	shutdown       func()
	purgeShutdown  func()
	deleteShutdown func()
}

// Registry is a thread-safe space -> spaceHandle map: the live set of
// supervisors a multi-space network node (ServeNetMulti) can dispatch to. A
// node registers a space when it starts materialising it and deregisters it
// on teardown, so a request for a torn-down space gets a clean 404 instead of
// hitting a stale Supervisor.
type Registry struct {
	mu     sync.RWMutex
	spaces map[string]spaceHandle
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{spaces: map[string]spaceHandle{}}
}

// Register adds (or replaces) the handle for space. Called once a space's
// Supervisor is constructed and ready to accept requests.
func (r *Registry) Register(space string, sup Supervisor, shutdown func()) {
	r.RegisterWithPurge(space, sup, shutdown, nil)
}

// RegisterWithPurge is Register with the deletion variant of the teardown, so
// POST /v1/down can tell a stop from a delete.
func (r *Registry) RegisterWithPurge(space string, sup Supervisor, shutdown, purgeShutdown func()) {
	r.RegisterWithDelete(space, sup, shutdown, purgeShutdown, nil)
}

// RegisterWithDelete is RegisterWithPurge plus the variant that also drops the
// space's named volumes, so POST /v1/down can tell a delete that keeps the
// data from one that destroys it.
func (r *Registry) RegisterWithDelete(space string, sup Supervisor, shutdown, purgeShutdown, deleteShutdown func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spaces[space] = spaceHandle{
		sup:            sup,
		shutdown:       shutdown,
		purgeShutdown:  purgeShutdown,
		deleteShutdown: deleteShutdown,
	}
}

// Unregister drops space from the registry. Called when a space's supervisor
// tears down, so requests for it 404 instead of reaching a dead Supervisor.
func (r *Registry) Unregister(space string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.spaces, space)
}

// get returns the handle for space and whether it is registered.
func (r *Registry) get(space string) (spaceHandle, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.spaces[space]
	return h, ok
}

// Supervisor returns the Supervisor registered for space, or ok=false if it
// isn't. This is the read-only accessor the Network Controller (spacemanager/
// netctrl) polls to learn a service's real IP/Port/readiness (via
// Supervisor.Snapshot/AllReady) without space.Space ever calling out to it.
func (r *Registry) Supervisor(space string) (sup Supervisor, ok bool) {
	h, ok := r.get(space)
	if !ok {
		return nil, false
	}
	return h.sup, true
}

// Names returns every currently-registered space name, unordered.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.spaces))
	for name := range r.spaces {
		names = append(names, name)
	}
	return names
}
