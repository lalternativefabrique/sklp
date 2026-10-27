package space

import "sync"

// hostMuxSingleton ref-counts ONE host mux per process. A process is one running
// `sklp dev` (one worktree), so its mux owns a single per-worktree port derived
// from the scope. The ref count covers the intra-process case (several stacks in
// one dev); two `sklp dev` worktrees are two PROCESSES, each with its OWN port —
// they cannot and need not share this in-memory singleton (their routes point at
// distinct in-process *Space objects). Per-worktree ports are what keep N
// worktrees host-reachable at once.
var hostMuxSingleton = struct {
	mu   sync.Mutex
	mux  *hostMux
	refs int
}{}

// AcquireHostMux returns the process host mux, starting its listener on the first
// acquire. basePort is the per-worktree derived start port (HostMuxPortFor); the
// bind walks +1 on collision. Each successful acquire MUST be paired with one
// ReleaseHostMux. A bind failure returns the error AND leaves the ref count
// untouched, so dev carries on without hostname ingress (services stay reachable
// east-west by name). The returned handle is the registration surface.
func AcquireHostMux(basePort int) (*HostMux, error) {
	hostMuxSingleton.mu.Lock()
	defer hostMuxSingleton.mu.Unlock()
	if hostMuxSingleton.mux != nil {
		hostMuxSingleton.refs++
		return &HostMux{m: hostMuxSingleton.mux}, nil
	}
	m := newHostMux(basePort)
	if err := m.start(); err != nil {
		return nil, err
	}
	hostMuxSingleton.mux = m
	hostMuxSingleton.refs = 1
	return &HostMux{m: m}, nil
}

// BoundPort returns the port the mux actually bound (for the printed URL), or 0
// if the handle is nil.
func (h *HostMux) BoundPort() int {
	if h == nil || h.m == nil {
		return 0
	}
	return h.m.boundPort()
}

// ReleaseHostMux drops one reference and closes the listener on the last release.
func ReleaseHostMux() {
	hostMuxSingleton.mu.Lock()
	defer hostMuxSingleton.mu.Unlock()
	if hostMuxSingleton.mux == nil {
		return
	}
	hostMuxSingleton.refs--
	if hostMuxSingleton.refs > 0 {
		return
	}
	hostMuxSingleton.mux.close()
	hostMuxSingleton.mux = nil
}

// HostMux is the caller-facing handle to the shared mux: it registers and
// unregisters a hostname -> (space service, container port) route. The router is
// the *Space that owns the service (it does the netns splice via SendConnReplay).
type HostMux struct{ m *hostMux }

// RegisterHost routes host to service:port inside router's space. Called once a
// service is Ready. Idempotent.
func (h *HostMux) RegisterHost(host string, router *Space, service string, port int) {
	if h == nil || h.m == nil {
		return
	}
	h.m.register(host, route{router: router, service: service, port: port})
}

// UnregisterHost drops a hostname (service stopped). Idempotent.
func (h *HostMux) UnregisterHost(host string) {
	if h == nil || h.m == nil {
		return
	}
	h.m.unregister(host)
}
