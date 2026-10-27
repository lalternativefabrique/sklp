package spacemanager

import (
	"sync"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/space"
)

// ingressLauncher is an execLauncher that also implements ingressController, so
// the agent's "open ingress on first Ready" lifecycle is asserted without a real
// space network. It records every OpenIngress call.
type ingressLauncher struct {
	*execLauncher
	mu     sync.Mutex
	opened map[string][]space.PortMapping
	calls  int
}

func newIngressLauncher() *ingressLauncher {
	return &ingressLauncher{
		execLauncher: newExecLauncher(),
		opened:       map[string][]space.PortMapping{},
	}
}

func (l *ingressLauncher) OpenIngress(serviceName string, mappings []space.PortMapping) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.opened[serviceName] = mappings
	l.calls++
}

func (l *ingressLauncher) openedFor(name string) ([]space.PortMapping, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	m, ok := l.opened[name]
	return m, ok
}

func (l *ingressLauncher) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

// TestAgentOpensIngressOnReady asserts a service with ports gets its host
// listeners opened exactly once, with the parsed mappings, after it becomes
// Ready (here: launch-and-forget, Ready immediately), while a service without
// ports never triggers ingress.
func TestAgentOpensIngressOnReady(t *testing.T) {
	l := newIngressLauncher()
	desired := []Service{
		{Name: "web", Command: []string{"sleep", "60"}, Ports: []string{"8080:80", "8443:443"}},
		{Name: "worker", Command: []string{"sleep", "60"}}, // no ports
	}
	cancel, wg := runAgent(t, l, desired, testOpts())
	defer wg.Wait()
	defer cancel()

	waitFor(t, 3*time.Second, "web ingress opened", func() bool {
		_, ok := l.openedFor("web")
		return ok
	})

	maps, _ := l.openedFor("web")
	if len(maps) != 2 || maps[0] != (space.PortMapping{HostPort: 8080, ContainerPort: 80}) ||
		maps[1] != (space.PortMapping{HostPort: 8443, ContainerPort: 443}) {
		t.Fatalf("web mappings = %+v", maps)
	}
	if _, ok := l.openedFor("worker"); ok {
		t.Fatal("worker has no ports; ingress must not be opened for it")
	}

	// Idempotent: a brief steady state must not re-open ingress for web.
	before := l.callCount()
	time.Sleep(200 * time.Millisecond)
	if l.callCount() != before {
		t.Fatalf("ingress opened %d times after steady state, want %d (idempotent)", l.callCount(), before)
	}
}

// TestAgentReconfigureRefreshesIngress re-applies a live service with
// proxy_protocol added: the open ingress must receive the flagged mappings,
// since its listeners are never re-bound.
func TestAgentReconfigureRefreshesIngress(t *testing.T) {
	l := newIngressLauncher()
	web := Service{Name: "web", Command: []string{"sleep", "60"}, Ports: []string{"0.0.0.0:25:25", "8080:80"}}
	a := NewAgent(l, nil, []Service{web}, testOpts())
	cancel, wg := startAgent(t, a)
	defer wg.Wait()
	defer cancel()

	waitFor(t, 3*time.Second, "web ingress opened", func() bool {
		_, ok := l.openedFor("web")
		return ok
	})

	next := web
	next.ProxyProtocol = []int{25}
	if !a.ServiceDiffers(next) {
		t.Fatal("adding proxy_protocol must count as a change")
	}
	a.Reconfigure(next)

	waitFor(t, 3*time.Second, "ingress refreshed with the proxy flag", func() bool {
		maps, _ := l.openedFor("web")
		return len(maps) == 2 && maps[0].ProxyProtocol && !maps[1].ProxyProtocol
	})
}
