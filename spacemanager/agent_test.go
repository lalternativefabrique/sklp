package spacemanager

import (
	"context"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// countingExecLauncher wraps execLauncher to count Start calls per service, so
// tests can assert restarts happened without inspecting the process tree.
type countingExecLauncher struct {
	*execLauncher
	mu     sync.Mutex
	starts map[string]int
	pids   map[string][]int
}

func newCounting() *countingExecLauncher {
	return &countingExecLauncher{
		execLauncher: newExecLauncher(),
		starts:       make(map[string]int),
		pids:         make(map[string][]int),
	}
}

func (c *countingExecLauncher) Start(ctx context.Context, svc Service) (int, error) {
	pid, err := c.execLauncher.Start(ctx, svc)
	c.mu.Lock()
	c.starts[svc.Name]++
	if err == nil {
		c.pids[svc.Name] = append(c.pids[svc.Name], pid)
	}
	c.mu.Unlock()
	return pid, err
}

func (c *countingExecLauncher) startCount(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.starts[name]
}

func (c *countingExecLauncher) allPIDs(name string) []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int(nil), c.pids[name]...)
}

func testOpts() Options {
	// Short, deterministic backoff so crash-loop tests finish fast.
	return Options{
		BackoffBase: 50 * time.Millisecond,
		BackoffMax:  200 * time.Millisecond,
		StableAfter: 500 * time.Millisecond,
		Logger:      log.New(io.Discard, "", 0),
	}
}

func runAgent(t *testing.T, l Launcher, desired []Service, opts Options) (context.CancelFunc, *sync.WaitGroup) {
	t.Helper()
	return runAgentRC(t, l, nil, desired, opts)
}

// runAgentRC is runAgent with an injected readiness checker (nil = no gate).
func runAgentRC(t *testing.T, l Launcher, rc readyChecker, desired []Service, opts Options) (context.CancelFunc, *sync.WaitGroup) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = NewAgent(l, rc, desired, opts).Run(ctx)
	}()
	return cancel, &wg
}

// waitFor polls cond until true or the deadline, failing the test on timeout.
func waitFor(t *testing.T, d time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", msg)
}

// TestDeathDetectedAndRestarted: a service whose process exits quickly is
// detected (pidfd) and relaunched.
func TestDeathDetectedAndRestarted(t *testing.T) {
	l := newCounting()
	desired := []Service{{Name: "blip", Command: []string{"sh", "-c", "sleep 0.1"}}}
	cancel, wg := runAgent(t, l, desired, testOpts())
	defer wg.Wait()
	defer cancel()

	// First launch happens immediately; after ~100ms it exits and is relaunched.
	// The agent launches instances under per-instance ids (<service>-<i>).
	waitFor(t, 3*time.Second, "service relaunched at least twice", func() bool {
		return l.startCount("blip-0") >= 2
	})

	// Distinct PIDs prove a genuine relaunch, not a double-count.
	pids := l.allPIDs("blip-0")
	if len(pids) >= 2 && pids[0] == pids[1] {
		t.Fatalf("expected distinct PIDs across restarts, got %v", pids)
	}
}

// TestBackoffGrows: a service that crashes immediately should be relaunched
// with an increasing delay (backoff), so the number of restarts in a window is
// bounded — far below what a no-backoff spin would produce.
func TestBackoffGrows(t *testing.T) {
	l := newCounting()
	desired := []Service{{Name: "crash", Command: []string{"sh", "-c", "exit 1"}}}
	opts := testOpts() // base 50ms, max 200ms
	cancel, wg := runAgent(t, l, desired, opts)
	defer wg.Wait()
	defer cancel()

	// Over ~700ms the delays are ~50,100,200,200,200 => ~5-6 starts. A
	// no-backoff loop would produce hundreds. Assert it stays bounded.
	time.Sleep(700 * time.Millisecond)
	got := l.startCount("crash-0")
	if got < 3 {
		t.Fatalf("expected several backed-off restarts, got %d", got)
	}
	if got > 12 {
		t.Fatalf("restarts not backed off (spinning?): %d in 700ms", got)
	}
}

// TestReapingNoZombie: after a managed child dies, the reaper must have
// wait()ed it — so the PID is no longer a (zombie) child. kill(pid,0) would
// still succeed on a zombie; ESRCH proves the slot was reclaimed.
func TestReapingNoZombie(t *testing.T) {
	l := newCounting()
	desired := []Service{{Name: "once", Command: []string{"sh", "-c", "sleep 0.05"}}}
	cancel, wg := runAgent(t, l, desired, testOpts())
	defer wg.Wait()
	defer cancel()

	waitFor(t, 3*time.Second, "first instance launched", func() bool {
		return len(l.allPIDs("once-0")) >= 1
	})
	firstPID := l.allPIDs("once-0")[0]

	// Once it has been relaunched, the first PID is dead AND reaped. A zombie
	// would still answer kill(pid,0); a reaped PID returns ESRCH.
	waitFor(t, 3*time.Second, "first instance reaped (relaunch observed)", func() bool {
		return l.startCount("once-0") >= 2
	})
	waitFor(t, 1*time.Second, "first PID fully reaped (ESRCH)", func() bool {
		return unix.Kill(firstPID, 0) == unix.ESRCH
	})
}

// TestTeardownStopsAll: on ctx cancel, every managed service is stopped and
// Run returns. We assert Stop was called for each service and the loop exits.
func TestTeardownStopsAll(t *testing.T) {
	stopped := &stopRecorder{Launcher: newExecLauncher(), seen: map[string]bool{}}
	desired := []Service{
		{Name: "a", Command: []string{"sh", "-c", "sleep 5"}},
		{Name: "b", Command: []string{"sh", "-c", "sleep 5"}},
	}
	cancel, wg := runAgent(t, stopped, desired, testOpts())

	waitFor(t, 3*time.Second, "both services started", func() bool {
		return stopped.started.Load() >= 2
	})

	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancel (teardown hung)")
	}

	if !stopped.wasStopped("a-0") || !stopped.wasStopped("b-0") {
		t.Fatalf("expected both instances stopped on teardown, got %v", stopped.seen)
	}
}

// TestOnAllReadyFiresOnceAtAllReady: OnAllReady is invoked exactly once, the
// first time every service has its full replica count Ready — the cue the CLI
// uses for the supervision banner. Services with no probe go Ready on launch.
func TestOnAllReadyFiresOnceAtAllReady(t *testing.T) {
	var fired atomic.Int32
	opts := testOpts()
	opts.OnAllReady = func() { fired.Add(1) }
	desired := []Service{
		{Name: "a", Command: []string{"sh", "-c", "sleep 5"}},
		{Name: "b", Command: []string{"sh", "-c", "sleep 5"}},
	}
	cancel, wg := runAgent(t, newExecLauncher(), desired, opts)
	defer wg.Wait()
	defer cancel()

	waitFor(t, 3*time.Second, "OnAllReady fired", func() bool {
		return fired.Load() >= 1
	})
	// Give the reconcile loop time to (wrongly) re-fire; it must not.
	time.Sleep(200 * time.Millisecond)
	if got := fired.Load(); got != 1 {
		t.Fatalf("OnAllReady fired %d times, want exactly 1", got)
	}
}

// TestOnAllReadyNotFiredWhenServiceNeverReady: a service that never reaches
// Ready (the agent keeps it out of AllReady) must keep OnAllReady silent.
func TestOnAllReadyNotFiredWhenServiceNeverReady(t *testing.T) {
	var fired atomic.Int32
	opts := testOpts()
	opts.OnAllReady = func() { fired.Add(1) }
	// A readiness checker that never returns ready holds the set below AllReady.
	rc := neverReadyChecker{}
	desired := []Service{{Name: "stuck", Command: []string{"sh", "-c", "sleep 5"}, Ready: &Probe{}}}
	cancel, wg := runAgentRC(t, newExecLauncher(), rc, desired, opts)
	defer wg.Wait()
	defer cancel()

	time.Sleep(300 * time.Millisecond)
	if got := fired.Load(); got != 0 {
		t.Fatalf("OnAllReady fired %d times for a never-ready set, want 0", got)
	}
}

// neverReadyChecker blocks until ctx is cancelled, never reporting ready.
type neverReadyChecker struct{}

func (neverReadyChecker) WaitReady(ctx context.Context, id string, p *Probe) error {
	<-ctx.Done()
	return ctx.Err()
}

func (neverReadyChecker) CheckReady(context.Context, string, *Probe) error { return nil }

// stopRecorder wraps a Launcher to record Stop calls and count Starts.
type stopRecorder struct {
	Launcher
	mu      sync.Mutex
	seen    map[string]bool
	started atomic.Int32
}

func (s *stopRecorder) Start(ctx context.Context, svc Service) (int, error) {
	pid, err := s.Launcher.Start(ctx, svc)
	if err == nil {
		s.started.Add(1)
	}
	return pid, err
}

func (s *stopRecorder) Stop(name string) {
	s.mu.Lock()
	s.seen[name] = true
	s.mu.Unlock()
	s.Launcher.Stop(name)
}

func (s *stopRecorder) wasStopped(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen[name]
}

// TestReconcile_ScaleDownStopsSurplus guards the scale-down fix: lowering a
// service's Replicas must STOP the surplus instances (which detaches them from
// the bridge and unregisters their DNS), not leave them running. Before the fix
// the reconcile created desired slots but never tore down the extras, so a 2→1
// scale-down left instance "api-1" alive and still load-balanced.
func TestReconcile_ScaleDownStopsSurplus(t *testing.T) {
	stopped := &stopRecorder{Launcher: newExecLauncher(), seen: map[string]bool{}}
	desired := []Service{
		{Name: "api", Replicas: 2, Command: []string{"sh", "-c", "sleep 5"}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agent := NewAgent(stopped, nil, desired, testOpts())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = agent.Run(ctx) }()

	// Both replicas launched.
	waitFor(t, 3*time.Second, "2 replicas started", func() bool {
		return stopped.started.Load() >= 2
	})

	// Scale down to 1.
	agent.Reconfigure(Service{Name: "api", Replicas: 1, Command: []string{"sh", "-c", "sleep 5"}})

	// The surplus instance (api-1) must be stopped; api-0 must survive.
	waitFor(t, 3*time.Second, "surplus replica api-1 stopped", func() bool {
		return stopped.wasStopped("api-1")
	})
	if stopped.wasStopped("api-0") {
		t.Errorf("api-0 should survive a 2→1 scale-down, but it was stopped")
	}

	cancel()
	wg.Wait()
}
