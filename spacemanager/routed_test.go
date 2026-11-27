package spacemanager

import (
	"sync"
	"testing"
	"time"
)

type pinLauncher struct {
	*execLauncher
	mu     sync.Mutex
	routed map[string][]string
	pushes int
}

func newPinLauncher() *pinLauncher {
	return &pinLauncher{execLauncher: newExecLauncher(), routed: map[string][]string{}}
}

func (l *pinLauncher) IPPort(id string) (string, int, bool) {
	return "ip-" + id, 8080, true
}

func (l *pinLauncher) RouteService(name string, ips []string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.routed[name] = append([]string(nil), ips...)
	l.pushes++
	return nil
}

func (l *pinLauncher) routesOnly(name string, ips ...string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	got, ok := l.routed[name]
	if !ok || len(got) != len(ips) {
		return false
	}
	for i := range ips {
		if got[i] != ips[i] {
			return false
		}
	}
	return true
}

func startPinAgent(t *testing.T, soak time.Duration, v2Cmd string) (*Agent, *pinLauncher) {
	t.Helper()
	rc := newFakeChecker(30 * time.Millisecond)
	rc.set("api-0", "pass")
	rc.set("api-v1-0", "pass")
	probe := &Probe{TCP: "127.0.0.1:1"}
	l := newPinLauncher()
	opts := proxyOpts(nil, newFakeProxy(), time.Minute)
	opts.Soak = soak
	opts.Drain = 0
	a := NewAgent(l, rc, []Service{rolloutSpec("v1", "sleep 30", probe)}, opts)
	cancel, wg := startAgent(t, a)
	t.Cleanup(func() { cancel(); wg.Wait() })
	waitFor(t, 3*time.Second, "v1 routed", func() bool {
		return a.AllReady() && l.routesOnly("api", "ip-api-0")
	})
	a.Reconfigure(rolloutSpec("v2", v2Cmd, probe))
	return a, l
}

func TestRouted_CutoverRoutesOnlyTheNewGenerationThroughTheSoak(t *testing.T) {
	a, l := startPinAgent(t, 600*time.Millisecond, "sleep 30")

	waitFor(t, 3*time.Second, "cutover routes only v2", func() bool {
		return l.routesOnly("api", "ip-api-v1-0")
	})
	if phase := rolloutPhaseOf(a, "api"); phase != RolloutSoaking {
		t.Fatalf("phase = %q, want the previous generation still alive in the soak", phase)
	}
	waitFor(t, 4*time.Second, "rollout complete", func() bool {
		rec, ok := lastRollout(a, "api")
		return ok && rec.Result == RolloutSucceeded
	})
	if !l.routesOnly("api", "ip-api-v1-0") {
		t.Fatal("v2 must stay the only routed generation after the rollout")
	}
}

func TestRouted_NewGenerationIsNotRoutedBeforeReady(t *testing.T) {
	rc := newFakeChecker(30 * time.Millisecond)
	rc.set("api-0", "pass")
	rc.set("api-v1-0", "block")
	probe := &Probe{TCP: "127.0.0.1:1"}
	l := newPinLauncher()
	a := NewAgent(l, rc, []Service{rolloutSpec("v1", "sleep 30", probe)}, proxyOpts(nil, newFakeProxy(), time.Minute))
	cancel, wg := startAgent(t, a)
	t.Cleanup(func() { cancel(); wg.Wait() })
	waitFor(t, 3*time.Second, "v1 routed", func() bool { return l.routesOnly("api", "ip-api-0") })

	a.Reconfigure(rolloutSpec("v2", "sleep 30", probe))
	waitFor(t, 3*time.Second, "v2 started", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		inst := a.instances["api-v1-0"]
		return inst != nil && inst.pid != 0
	})
	time.Sleep(300 * time.Millisecond)
	if !l.routesOnly("api", "ip-api-0") {
		t.Fatal("an instance that is not Ready must receive no traffic")
	}
}

func TestRouted_CrashDuringSoakRoutesBackToThePreviousGeneration(t *testing.T) {
	a, l := startPinAgent(t, 5*time.Second, "sleep 0.4; exit 3")

	waitFor(t, 3*time.Second, "cutover routes only v2", func() bool {
		return l.routesOnly("api", "ip-api-v1-0")
	})
	waitFor(t, 4*time.Second, "routed back to v1", func() bool {
		return l.routesOnly("api", "ip-api-0")
	})
	rec, ok := lastRollout(a, "api")
	if !ok || rec.Result != RolloutRolledBack {
		t.Fatalf("expected a rolled_back record, got %+v", rec)
	}
}
