package spacemanager

import (
	"fmt"
	"sync"
	"testing"
)

// gwBackendLauncher is an execLauncher that also implements gatewayBackender,
// so the agent's gateway-backend resolution is asserted without a real space
// netns. It hands back a stable "127.0.0.1:<port>" per service (the loopback the
// real launcher would allocate) and records the resolved container ports.
type gwBackendLauncher struct {
	*execLauncher
	mu      sync.Mutex
	ports   map[string]int // service -> container port seen
	hostSeq int            // synthetic host-port counter, memoised per service
	hostOf  map[string]int
}

func newGWBackendLauncher() *gwBackendLauncher {
	return &gwBackendLauncher{
		execLauncher: newExecLauncher(),
		ports:        map[string]int{},
		hostSeq:      40000,
		hostOf:       map[string]int{},
	}
}

func (l *gwBackendLauncher) GatewayBackend(serviceName string, containerPort int) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ports[serviceName] = containerPort
	hp, ok := l.hostOf[serviceName]
	if !ok {
		l.hostSeq++
		hp = l.hostSeq
		l.hostOf[serviceName] = hp // memoised: idempotent per service
	}
	return fmt.Sprintf("127.0.0.1:%d", hp), nil
}

func (l *gwBackendLauncher) portFor(name string) (int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.ports[name]
	return p, ok
}

// TestGatewayBackendsForDomainedService asserts gatewayBackendsLocked returns
// the launcher's single loopback backend for a service carrying a gateway route
// (passing the Expose port through), and nil for a service with no route.
func TestGatewayBackendsForDomainedService(t *testing.T) {
	l := newGWBackendLauncher()
	desired := []Service{
		{Name: "core", Port: 4000},
		{Name: "worker", Port: 9000}, // no route
	}
	opts := testOpts()
	opts.Routes = []ResolvedRoute{{Service: "core", Host: "api.example.com"}}
	a := NewAgent(l, nil, desired, opts)

	a.mu.Lock()
	got := a.gatewayBackendsLocked("core")
	none := a.gatewayBackendsLocked("worker")
	a.mu.Unlock()

	if len(got) != 1 || got[0] != "127.0.0.1:40001" {
		t.Fatalf("routed backend = %v, want [127.0.0.1:40001]", got)
	}
	if none != nil {
		t.Fatalf("unrouted service must yield nil backends, got %v", none)
	}
	// The Expose port is what the gateway ingress targets in the container.
	if p, ok := l.portFor("core"); !ok || p != 4000 {
		t.Fatalf("container port passed to launcher = %d (ok=%v), want 4000", p, ok)
	}

	// Idempotent: a second resolution returns the same memoised loopback addr.
	a.mu.Lock()
	again := a.gatewayBackendsLocked("core")
	a.mu.Unlock()
	if len(again) != 1 || again[0] != got[0] {
		t.Fatalf("second resolution = %v, want stable %v", again, got)
	}
}

// TestGatewayBackendsNilWithoutLauncherSupport asserts a launcher that does NOT
// implement gatewayBackender (the plain exec launcher) leaves the agent's
// gateway path inert: gatewayBackendsLocked returns nil even for a routed
// service, so the exec/test path needs no real netns.
func TestGatewayBackendsNilWithoutLauncherSupport(t *testing.T) {
	l := newExecLauncher()
	desired := []Service{{Name: "core", Port: 4000}}
	opts := testOpts()
	opts.Routes = []ResolvedRoute{{Service: "core", Host: "api.example.com"}}
	a := NewAgent(l, nil, desired, opts)

	a.mu.Lock()
	got := a.gatewayBackendsLocked("core")
	a.mu.Unlock()
	if got != nil {
		t.Fatalf("no gatewayBackender support must yield nil, got %v", got)
	}
}
