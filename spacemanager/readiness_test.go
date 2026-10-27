package spacemanager

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeChecker is an injectable readyChecker for tests: no network, no
// container. Per service, its behaviour is "pass" (ready after `delay`),
// "fail" (error after `delay`), or "block" (ready only when released, else
// honour ctx cancellation). It records how many waits were cancelled so the
// death-during-readiness race can be asserted.
type fakeChecker struct {
	mu        sync.Mutex
	behaviour map[string]string // name -> pass|fail|block
	delay     time.Duration
	release   chan struct{} // closed to unblock "block" services
	cancelled map[string]int
	calls     map[string]int
	unhealthy map[string]bool
	checks    map[string]int
}

func newFakeChecker(delay time.Duration) *fakeChecker {
	return &fakeChecker{
		behaviour: map[string]string{},
		delay:     delay,
		release:   make(chan struct{}),
		cancelled: map[string]int{},
		calls:     map[string]int{},
		unhealthy: map[string]bool{},
		checks:    map[string]int{},
	}
}

func (f *fakeChecker) setHealthy(name string, healthy bool) {
	f.mu.Lock()
	f.unhealthy[name] = !healthy
	f.mu.Unlock()
}

func (f *fakeChecker) checkCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checks[name]
}

func (f *fakeChecker) CheckReady(_ context.Context, name string, _ *Probe) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks[name]++
	if f.unhealthy[name] {
		return fmt.Errorf("probe failed for %q", name)
	}
	return nil
}

func (f *fakeChecker) set(name, b string) {
	f.mu.Lock()
	f.behaviour[name] = b
	f.mu.Unlock()
}

func (f *fakeChecker) cancelCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancelled[name]
}

func (f *fakeChecker) WaitReady(ctx context.Context, name string, _ *Probe) error {
	f.mu.Lock()
	f.calls[name]++
	b := f.behaviour[name]
	f.mu.Unlock()

	switch b {
	case "block":
		select {
		case <-f.release:
			return nil
		case <-ctx.Done():
			f.mu.Lock()
			f.cancelled[name]++
			f.mu.Unlock()
			return ctx.Err()
		}
	case "fail":
		select {
		case <-time.After(f.delay):
			return fmt.Errorf("probe failed for %q", name)
		case <-ctx.Done():
			f.mu.Lock()
			f.cancelled[name]++
			f.mu.Unlock()
			return ctx.Err()
		}
	default: // pass
		select {
		case <-time.After(f.delay):
			return nil
		case <-ctx.Done():
			f.mu.Lock()
			f.cancelled[name]++
			f.mu.Unlock()
			return ctx.Err()
		}
	}
}

// recorder is a thread-safe StateObserver capturing every transition.
type recorder struct {
	mu     sync.Mutex
	events []StateEvent
}

func (r *recorder) observe(e StateEvent) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

// saw reports whether a from->to transition was recorded for name.
func (r *recorder) saw(name string, from, to State) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e.Service == name && e.From == from && e.To == to {
			return true
		}
	}
	return false
}

func (r *recorder) count(name string, to State) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e.Service == name && e.To == to {
			n++
		}
	}
	return n
}

func readyOpts(rec *recorder) Options {
	o := testOpts()
	if rec != nil {
		o.Observer = rec.observe
	}
	return o
}

// TestReadyGatePass: a service with a probe that passes transitions
// Starting -> Ready, and is only counted satisfied after Ready.
func TestReadyGatePass(t *testing.T) {
	l := newCounting()
	rc := newFakeChecker(150 * time.Millisecond)
	rc.set("api-0", "pass")
	rec := &recorder{}
	desired := []Service{{Name: "api", Command: []string{"sh", "-c", "sleep 5"}, Ready: &Probe{TCP: "127.0.0.1:1"}}}

	a := NewAgent(l, rc, desired, readyOpts(rec))
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = a.Run(ctx) }()
	defer wg.Wait()
	defer cancel()

	// Starting must precede Ready; not satisfied during the gate window.
	waitFor(t, time.Second, "entered Starting", func() bool { return rec.saw("api", Pending, Starting) })
	if a.AllReady() {
		t.Fatal("service counted ready before probe passed")
	}
	waitFor(t, 3*time.Second, "became Ready", func() bool { return a.State("api") == Ready })
	if !rec.saw("api", Starting, Ready) {
		t.Fatal("missing Starting->Ready transition")
	}
	if !a.AllReady() {
		t.Fatal("AllReady false after probe passed")
	}
}

// TestReadyGateNilImmediate: a service with no probe is Ready immediately on
// launch (milestone-1 behaviour preserved), no gate.
func TestReadyGateNilImmediate(t *testing.T) {
	l := newCounting()
	rc := newFakeChecker(time.Second) // would block if ever consulted
	rec := &recorder{}
	desired := []Service{{Name: "plain", Command: []string{"sh", "-c", "sleep 5"}}} // Ready nil

	a := NewAgent(l, rc, desired, readyOpts(rec))
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = a.Run(ctx) }()
	defer wg.Wait()
	defer cancel()

	waitFor(t, 2*time.Second, "ready without probe", func() bool { return a.State("plain") == Ready })
	rc.mu.Lock()
	calls := rc.calls["plain-0"]
	rc.mu.Unlock()
	if calls != 0 {
		t.Fatalf("checker consulted for a nil-probe service (%d calls)", calls)
	}
}

// TestReadyGateFailRestart: a service whose probe fails transitions to Failed
// and is restarted with backoff (it is alive but never ready -> a failure,
// not silently abandoned).
func TestReadyGateFailRestart(t *testing.T) {
	l := newCounting()
	rc := newFakeChecker(50 * time.Millisecond)
	rc.set("bad-0", "fail")
	rec := &recorder{}
	desired := []Service{{Name: "bad", Command: []string{"sh", "-c", "sleep 5"}, Ready: &Probe{TCP: "127.0.0.1:1"}}}

	a := NewAgent(l, rc, desired, readyOpts(rec))
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = a.Run(ctx) }()
	defer wg.Wait()
	defer cancel()

	// Probe fails -> Failed -> Restarting -> relaunched (>=2 starts).
	waitFor(t, 3*time.Second, "probe failure observed", func() bool { return rec.saw("bad", Starting, Failed) })
	waitFor(t, 3*time.Second, "restart scheduled", func() bool { return rec.count("bad", Restarting) >= 1 })
	waitFor(t, 5*time.Second, "relaunched after readiness failure", func() bool { return l.startCount("bad-0") >= 2 })
	if a.AllReady() {
		t.Fatal("a never-ready service must not be counted satisfied")
	}
}

// TestDeathDuringReadiness is the critical race: the init dies WHILE we await
// its readiness. The in-flight WaitReady must be cancelled and the service must
// take the death/restart path — never marked Ready. Uses a "block" checker
// (only released manually) and an exec process that exits fast.
func TestDeathDuringReadiness(t *testing.T) {
	l := newCounting()
	rc := newFakeChecker(0)
	rc.set("racer-0", "block") // never readies on its own; only ctx-cancel ends it
	rec := &recorder{}
	// Process dies ~80ms after launch, well before any manual release.
	desired := []Service{{Name: "racer", Command: []string{"sh", "-c", "sleep 0.08"}, Ready: &Probe{TCP: "127.0.0.1:1"}}}

	a := NewAgent(l, rc, desired, readyOpts(rec))
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = a.Run(ctx) }()
	defer wg.Wait()
	defer cancel()

	// The death must cancel the readiness wait at least once and drive a
	// restart. The service must never reach Ready (the probe never passed).
	waitFor(t, 3*time.Second, "readiness wait cancelled by death", func() bool { return rc.cancelCount("racer-0") >= 1 })
	waitFor(t, 5*time.Second, "relaunched after death-during-readiness", func() bool { return l.startCount("racer-0") >= 2 })

	if rec.count("racer", Ready) != 0 {
		t.Fatalf("dead-before-ready service was marked Ready (%d times)", rec.count("racer", Ready))
	}
	if a.State("racer") == Ready {
		t.Fatal("service in Ready state after dying mid-readiness")
	}
}

// TestReadyMonitorDegradesAndRecovers: a Ready instance whose probe starts
// failing after boot leaves Ready (Degraded, out of AllReady) without a
// restart, and comes back to Ready once the probe passes again.
func TestReadyMonitorDegradesAndRecovers(t *testing.T) {
	l := newCounting()
	rc := newFakeChecker(10 * time.Millisecond)
	rc.set("api-0", "pass")
	rec := &recorder{}
	desired := []Service{{Name: "api", Command: []string{"sh", "-c", "sleep 5"}, Ready: &Probe{TCP: "127.0.0.1:1"}}}
	opts := readyOpts(rec)
	opts.ProbeInterval = 20 * time.Millisecond
	opts.ProbeFailures = 3

	a := NewAgent(l, rc, desired, opts)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = a.Run(ctx) }()
	defer wg.Wait()
	defer cancel()

	waitFor(t, 3*time.Second, "became Ready", func() bool { return a.State("api") == Ready })
	waitFor(t, 2*time.Second, "monitor probing", func() bool { return rc.checkCount("api-0") >= 2 })
	if a.State("api") != Ready {
		t.Fatalf("passing checks moved the service out of Ready: %s", a.State("api"))
	}

	rc.setHealthy("api-0", false)
	waitFor(t, 3*time.Second, "degraded", func() bool { return a.State("api") == Degraded })
	if !rec.saw("api", Ready, Degraded) {
		t.Fatal("missing Ready->Degraded transition")
	}
	if a.AllReady() {
		t.Fatal("AllReady true while degraded")
	}
	if l.startCount("api-0") != 1 {
		t.Fatalf("degraded instance was relaunched (%d starts)", l.startCount("api-0"))
	}

	rc.setHealthy("api-0", true)
	waitFor(t, 3*time.Second, "recovered", func() bool { return a.State("api") == Ready })
	if !rec.saw("api", Degraded, Ready) {
		t.Fatal("missing Degraded->Ready transition")
	}
	if l.startCount("api-0") != 1 {
		t.Fatalf("recovery relaunched the instance (%d starts)", l.startCount("api-0"))
	}
}

// TestReadyMonitorBelowThresholdStaysReady: fewer consecutive failures than
// the threshold never leave Ready.
func TestReadyMonitorBelowThresholdStaysReady(t *testing.T) {
	l := newCounting()
	rc := newFakeChecker(10 * time.Millisecond)
	rc.set("api-0", "pass")
	rec := &recorder{}
	desired := []Service{{Name: "api", Command: []string{"sh", "-c", "sleep 5"}, Ready: &Probe{TCP: "127.0.0.1:1"}}}
	opts := readyOpts(rec)
	opts.ProbeInterval = 20 * time.Millisecond
	opts.ProbeFailures = 3

	a := NewAgent(l, rc, desired, opts)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = a.Run(ctx) }()
	defer wg.Wait()
	defer cancel()

	waitFor(t, 3*time.Second, "became Ready", func() bool { return a.State("api") == Ready })
	before := rc.checkCount("api-0")
	rc.setHealthy("api-0", false)
	waitFor(t, 2*time.Second, "two failed checks", func() bool { return rc.checkCount("api-0") >= before+2 })
	rc.setHealthy("api-0", true)
	waitFor(t, 2*time.Second, "checks resumed", func() bool { return rc.checkCount("api-0") >= before+5 })
	if rec.count("api", Degraded) != 0 {
		t.Fatal("degraded below the failure threshold")
	}
}

// TestReadyMonitorStopsOnDeath: the monitor of a dead instance is cancelled
// with it, so no check runs against a relaunch it does not own.
func TestReadyMonitorStopsOnDeath(t *testing.T) {
	l := newCounting()
	rc := newFakeChecker(10 * time.Millisecond)
	rc.set("api-0", "pass")
	desired := []Service{{Name: "api", Command: []string{"sh", "-c", "sleep 0.3"}, Ready: &Probe{TCP: "127.0.0.1:1"}}}
	opts := readyOpts(nil)
	opts.ProbeInterval = 20 * time.Millisecond

	a := NewAgent(l, rc, desired, opts)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = a.Run(ctx) }()
	defer wg.Wait()
	defer cancel()

	waitFor(t, 3*time.Second, "relaunched after death", func() bool { return l.startCount("api-0") >= 2 })
	waitFor(t, 3*time.Second, "second run Ready", func() bool { return a.State("api") == Ready })
	rc.setHealthy("api-0", false)
	waitFor(t, 3*time.Second, "second run degraded by its own monitor", func() bool { return a.State("api") == Degraded })
}
