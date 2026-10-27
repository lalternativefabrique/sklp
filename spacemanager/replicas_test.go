package spacemanager

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// killPID sends SIGKILL to pid; the agent must detect the death via pidfd.
func killPID(t *testing.T, pid int) {
	t.Helper()
	if err := unix.Kill(pid, unix.SIGKILL); err != nil && err != unix.ESRCH {
		t.Fatalf("kill %d: %v", pid, err)
	}
}

// fakeProxy is an injectable proxyController for tests: it records every
// SetBackends call so the tests can assert which addresses are routed to a
// service and in what order — without any real traefik, file, or network.
type fakeProxy struct {
	mu       sync.Mutex
	current  map[string][]string   // service -> last backend set
	calls    map[string][][]string // service -> history of backend sets
	setAtLen map[string]int        // service -> SetBackends call count
}

func newFakeProxy() *fakeProxy {
	return &fakeProxy{
		current:  map[string][]string{},
		calls:    map[string][][]string{},
		setAtLen: map[string]int{},
	}
}

func (p *fakeProxy) SetBackends(service string, addrs []string) error {
	cp := append([]string(nil), addrs...)
	sort.Strings(cp)
	p.mu.Lock()
	p.current[service] = cp
	p.calls[service] = append(p.calls[service], cp)
	p.setAtLen[service]++
	p.mu.Unlock()
	return nil
}

func (p *fakeProxy) backends(service string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.current[service]...)
}

func (p *fakeProxy) has(service, addr string) bool {
	for _, a := range p.backends(service) {
		if a == addr {
			return true
		}
	}
	return false
}

func proxyOpts(rec *recorder, px proxyController, rollout time.Duration) Options {
	o := testOpts()
	if rec != nil {
		o.Observer = rec.observe
	}
	o.Proxy = px
	o.RolloutTimeout = rollout
	o.Soak = 200 * time.Millisecond
	return o
}

func startAgent(t *testing.T, a *Agent) (context.CancelFunc, *sync.WaitGroup) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = a.Run(ctx) }()
	return cancel, &wg
}

// TestReplicasAllReadyAndRestart: a service with Replicas=3 launches three
// independent instances; all three reach AllReady. Killing one instance's
// process gets it relaunched (per-instance restart), and the service returns
// to fully Ready.
func TestReplicasAllReadyAndRestart(t *testing.T) {
	l := newCounting()
	desired := []Service{{
		Name:     "web",
		Command:  []string{"sh", "-c", "sleep 30"},
		Replicas: 3,
	}}
	a := NewAgent(l, nil, desired, testOpts())
	cancel, wg := startAgent(t, a)
	defer wg.Wait()
	defer cancel()

	waitFor(t, 3*time.Second, "three replicas all ready", func() bool {
		return a.AllReady() &&
			l.startCount("web-0") >= 1 &&
			l.startCount("web-1") >= 1 &&
			l.startCount("web-2") >= 1
	})

	// Kill replica web-1's current process; the agent must relaunch it.
	pids := l.allPIDs("web-1")
	if len(pids) == 0 {
		t.Fatal("web-1 never launched")
	}
	killPID(t, pids[len(pids)-1])

	waitFor(t, 5*time.Second, "dead replica relaunched", func() bool {
		return l.startCount("web-1") >= 2
	})
	waitFor(t, 5*time.Second, "service fully ready again", func() bool {
		return a.AllReady()
	})
}

// TestProxyRoutesReadyInstancesOnly: a proxied service's Ready instances appear
// in the proxy backend set with their addresses; when an instance dies it is
// removed from the backends BEFORE it is killed (assert via Stop ordering vs.
// the backend set).
func TestProxyRoutesReadyInstancesOnly(t *testing.T) {
	base := newExecLauncher()
	// orderRec records the relative order of "backend removed" vs. "instance
	// stopped" for the instance we will kill.
	or := &orderRecorder{}
	l := &orderingLauncher{Launcher: base, rec: or}
	px := &orderingProxy{fakeProxy: newFakeProxy(), rec: or}

	rc := newFakeChecker(20 * time.Millisecond)
	rc.set("svc-0", "pass")
	rc.set("svc-1", "pass")

	desired := []Service{{
		Name:     "svc",
		Command:  []string{"sh", "-c", "sleep 30"},
		Replicas: 2,
		Port:     8080,
		Ready:    &Probe{TCP: "127.0.0.1:1"},
	}}
	a := NewAgent(l, rc, desired, proxyOpts(nil, px, time.Minute))
	cancel, wg := startAgent(t, a)
	defer wg.Wait()
	defer cancel()

	waitFor(t, 3*time.Second, "both replicas routed", func() bool {
		return px.has("svc", "exec://svc-0") && px.has("svc", "exec://svc-1")
	})

	// Kill svc-1; the agent must drop exec://svc-1 from the backends BEFORE
	// calling Stop(svc-1).
	pids := l.Launcher.(*execLauncher).pidOf("svc-1")
	if pids == 0 {
		t.Fatal("svc-1 has no pid")
	}
	killPID(t, pids)

	waitFor(t, 5*time.Second, "dead instance removed from backends", func() bool {
		return !px.has("svc", "exec://svc-1")
	})

	if !or.removedBeforeStopped("svc-1") {
		t.Fatalf("svc-1 was stopped before being removed from the proxy backends (order=%v)", or.events())
	}
}

// TestRollingUpdateZeroDowntime: changing a service's image triggers a
// start-first rolling update — the new instance starts alongside the old, the
// proxy switches to the new instance only after it is Ready, and the old
// instance is stopped ONLY AFTER the cutover (verified: old instance alive
// until new is Ready).
func TestRollingUpdateZeroDowntime(t *testing.T) {
	base := newExecLauncher()
	stops := &stopRecorder{Launcher: base, seen: map[string]bool{}}
	// A readiness delay wide enough that the "v2 starting, v1 still serving"
	// window is observable by the test's polling.
	rc := newFakeChecker(250 * time.Millisecond)
	rc.set("app-0", "pass")    // v1 instance
	rc.set("app-v1-0", "pass") // v2 instance

	px := newFakeProxy()
	probe := &Probe{TCP: "127.0.0.1:1"}
	desired := []Service{{
		Name:    "app",
		Image:   "v1",
		Command: []string{"sh", "-c", "sleep 30"},
		Port:    8080,
		Ready:   probe,
	}}
	a := NewAgent(stops, rc, desired, proxyOpts(nil, px, time.Minute))
	cancel, wg := startAgent(t, a)
	defer wg.Wait()
	defer cancel()

	waitFor(t, 3*time.Second, "v1 ready and routed", func() bool {
		return a.AllReady() && px.has("app", "exec://app-0")
	})

	// Trigger the rolling update to v2.
	a.Reconfigure(Service{
		Name:    "app",
		Image:   "v2",
		Command: []string{"sh", "-c", "sleep 30"},
		Port:    8080,
		Ready:   probe,
	})

	// While v2 is starting (before it is Ready), v1 must stay alive AND routed.
	waitFor(t, 3*time.Second, "v2 instance started", func() bool {
		return len(stops.Launcher.(*execLauncher).pidsOf("app-v1-0")) >= 1
	})
	if stops.wasStopped("app-0") {
		t.Fatal("old instance stopped before new instance became Ready (downtime!)")
	}
	if !px.has("app", "exec://app-0") {
		t.Fatal("old instance removed from proxy before cutover (downtime!)")
	}

	// After v2 is Ready, the proxy cuts over and v1 is stopped.
	waitFor(t, 4*time.Second, "proxy cut over to v2", func() bool {
		return px.has("app", "exec://app-v1-0") && !px.has("app", "exec://app-0")
	})
	waitFor(t, 4*time.Second, "old instance stopped after cutover", func() bool {
		return stops.wasStopped("app-0")
	})
	if !a.AllReady() {
		t.Fatal("service not Ready after rollout")
	}
}

// TestSecondRolloutDoesNotClobberFirstGeneration is the regression test for
// the id-collision bug: a SECOND rollout of the same service must not reuse
// the instance ids the first rollout promoted into steady-state. A collision
// would overwrite the live, Ready v2 instance's bookkeeping entry the moment
// the third version starts — before it is ever readiness-checked — making the
// currently-serving instance invisible to the proxy and to AllReady, even
// though its process is still running and untouched.
func TestSecondRolloutDoesNotClobberFirstGeneration(t *testing.T) {
	base := newExecLauncher()
	stops := &stopRecorder{Launcher: base, seen: map[string]bool{}}
	rc := newFakeChecker(50 * time.Millisecond)
	rc.set("app-0", "pass")    // v1 instance
	rc.set("app-v1-0", "pass") // v2 instance (first rollout)
	rc.set("app-v2-0", "pass") // v3 instance (second rollout)

	px := newFakeProxy()
	probe := &Probe{TCP: "127.0.0.1:1"}
	desired := []Service{{
		Name:    "app",
		Image:   "v1",
		Command: []string{"sh", "-c", "sleep 30"},
		Port:    8080,
		Ready:   probe,
	}}
	a := NewAgent(stops, rc, desired, proxyOpts(nil, px, time.Minute))
	cancel, wg := startAgent(t, a)
	defer wg.Wait()
	defer cancel()

	waitFor(t, 3*time.Second, "v1 ready and routed", func() bool {
		return a.AllReady() && px.has("app", "exec://app-0")
	})

	// First rollout: v1 -> v2.
	a.Reconfigure(Service{
		Name:    "app",
		Image:   "v2",
		Command: []string{"sh", "-c", "sleep 30"},
		Port:    8080,
		Ready:   probe,
	})
	waitFor(t, 3*time.Second, "cut over to v2", func() bool {
		return px.has("app", "exec://app-v1-0") && !px.has("app", "exec://app-0") && a.AllReady()
	})

	// Second rollout: v2 -> v3. This must NOT lose v2's Ready state the moment
	// v3 starts — v2 is the currently-live generation and must stay routed and
	// Ready-counted until v3 is confirmed Ready.
	a.Reconfigure(Service{
		Name:    "app",
		Image:   "v3",
		Command: []string{"sh", "-c", "sleep 30"},
		Port:    8080,
		Ready:   probe,
	})
	waitFor(t, 2*time.Second, "v3 instance started", func() bool {
		return len(stops.Launcher.(*execLauncher).pidsOf("app-v2-0")) >= 1
	})
	if !a.AllReady() {
		t.Fatal("v2 lost its Ready state the moment the second rollout started (id collision clobbered its bookkeeping)")
	}
	if !px.has("app", "exec://app-v1-0") {
		t.Fatal("v2 was dropped from the proxy before the second rollout's cutover (downtime!)")
	}

	waitFor(t, 3*time.Second, "cut over to v3", func() bool {
		return px.has("app", "exec://app-v2-0") && !px.has("app", "exec://app-v1-0")
	})
	if !a.AllReady() {
		t.Fatal("service not Ready after the second rollout")
	}
}

// TestRollingUpdateFailureKeepsOld is the critical safety test: the new version
// never becomes Ready (its probe blocks until the rollout times out). The old
// instance must be KEPT alive, KEPT in the proxy backends, and never stopped;
// the rollout is reported Failed (rollback-on-failure).
func TestRollingUpdateFailureKeepsOld(t *testing.T) {
	base := newExecLauncher()
	stops := &stopRecorder{Launcher: base, seen: map[string]bool{}}
	rc := newFakeChecker(30 * time.Millisecond)
	rc.set("api-0", "pass")     // v1 readies
	rc.set("api-v1-0", "block") // v2 never readies on its own -> rollout times out

	px := newFakeProxy()
	rec := &recorder{}
	probe := &Probe{TCP: "127.0.0.1:1"}
	desired := []Service{{
		Name:    "api",
		Image:   "v1",
		Command: []string{"sh", "-c", "sleep 30"},
		Port:    8080,
		Ready:   probe,
	}}
	// Short rollout timeout so the test does not wait minutes.
	a := NewAgent(stops, rc, desired, proxyOpts(rec, px, 300*time.Millisecond))
	cancel, wg := startAgent(t, a)
	defer wg.Wait()
	defer cancel()

	waitFor(t, 3*time.Second, "v1 ready and routed", func() bool {
		return a.AllReady() && px.has("api", "exec://api-0")
	})

	a.Reconfigure(Service{
		Name:    "api",
		Image:   "v2",
		Command: []string{"sh", "-c", "sleep 30"},
		Port:    8080,
		Ready:   probe,
	})

	// Wait past the rollout timeout for the failure to be declared.
	waitFor(t, 5*time.Second, "rollout failure reported", func() bool {
		return rec.saw("api", Starting, Failed)
	})

	// Give the agent a moment to settle, then assert the safe outcome.
	time.Sleep(200 * time.Millisecond)

	if stops.wasStopped("api-0") {
		t.Fatal("old instance was stopped despite the rollout failing (NOT safe)")
	}
	if !px.has("api", "exec://api-0") {
		t.Fatal("old instance removed from proxy backends despite rollout failure")
	}
	if px.has("api", "exec://api-v1-0") {
		t.Fatal("never-ready new instance leaked into the proxy backends")
	}
	if !a.AllReady() {
		t.Fatal("old (kept) version must still be counted Ready after a failed rollout")
	}
}

// --- test helpers for ordering assertions ---

// orderRecorder records a sequence of (kind, target) events so a test can
// assert relative ordering (e.g. backend-removed before instance-stopped).
type orderRecorder struct {
	mu  sync.Mutex
	seq []orderEvent
}

type orderEvent struct {
	kind   string // "removed" | "stopped"
	target string
}

func (o *orderRecorder) record(kind, target string) {
	o.mu.Lock()
	o.seq = append(o.seq, orderEvent{kind, target})
	o.mu.Unlock()
}

func (o *orderRecorder) events() []orderEvent {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]orderEvent(nil), o.seq...)
}

// removedBeforeStopped reports whether the "removed from backends" event for
// addr exec://<id> precedes the "stopped" event for <id>.
func (o *orderRecorder) removedBeforeStopped(id string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	removed, stopped := -1, -1
	for i, e := range o.seq {
		if e.kind == "removed" && e.target == id && removed < 0 {
			removed = i
		}
		if e.kind == "stopped" && e.target == id && stopped < 0 {
			stopped = i
		}
	}
	return removed >= 0 && stopped >= 0 && removed < stopped
}

// orderingLauncher records each Stop in the order recorder.
type orderingLauncher struct {
	Launcher
	rec *orderRecorder
}

func (l *orderingLauncher) Stop(id string) {
	l.rec.record("stopped", id)
	l.Launcher.Stop(id)
}

// orderingProxy records, on each SetBackends, which previously-present
// addresses were removed.
type orderingProxy struct {
	*fakeProxy
	rec *orderRecorder
}

func (p *orderingProxy) SetBackends(service string, addrs []string) error {
	prev := p.fakeProxy.backends(service)
	_ = p.fakeProxy.SetBackends(service, addrs)
	now := map[string]bool{}
	for _, a := range addrs {
		now[a] = true
	}
	for _, a := range prev {
		if !now[a] {
			// addr is "exec://<id>"; record the bare id.
			p.rec.record("removed", idFromAddr(a))
		}
	}
	return nil
}

func idFromAddr(addr string) string {
	const prefix = "exec://"
	if len(addr) > len(prefix) && addr[:len(prefix)] == prefix {
		return addr[len(prefix):]
	}
	return addr
}

// slowStopLauncher holds Stop for delay and records when it returned and when
// each instance was started, so a test can assert on ORDER of return vs start.
type slowStopLauncher struct {
	Launcher
	delay      time.Duration
	mu         sync.Mutex
	stopReturn map[string]time.Time
	startAt    map[string]time.Time
}

func (l *slowStopLauncher) Start(ctx context.Context, svc Service) (int, error) {
	pid, err := l.Launcher.Start(ctx, svc)
	l.mu.Lock()
	l.startAt[svc.Name] = time.Now()
	l.mu.Unlock()
	return pid, err
}

func (l *slowStopLauncher) Stop(id string) {
	time.Sleep(l.delay)
	l.Launcher.Stop(id)
	l.mu.Lock()
	l.stopReturn[id] = time.Now()
	l.mu.Unlock()
}

func (l *slowStopLauncher) times(id string) (stopped, started time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stopReturn[id], l.startAt[id]
}

func statefulSpec(image string) Service {
	return Service{
		Name:    "db",
		Image:   image,
		Command: []string{"sh", "-c", "sleep 30"},
		Port:    5432,
		Ready:   &Probe{TCP: "127.0.0.1:1"},
		Volumes: []string{"pgdata:/var/lib/postgresql/data"},
	}
}

func TestStatefulReconfigureStopsTheOldWriterFirst(t *testing.T) {
	base := newExecLauncher()
	slow := &slowStopLauncher{Launcher: base, delay: 300 * time.Millisecond, stopReturn: map[string]time.Time{}, startAt: map[string]time.Time{}}
	rc := newFakeChecker(250 * time.Millisecond)
	rc.set("db-0", "pass")
	rc.set("db-v1-0", "pass")

	px := newFakeProxy()
	a := NewAgent(slow, rc, []Service{statefulSpec("v1")}, proxyOpts(nil, px, time.Minute))
	cancel, wg := startAgent(t, a)
	defer wg.Wait()
	defer cancel()

	waitFor(t, 3*time.Second, "v1 ready", func() bool { return a.AllReady() })

	a.Reconfigure(statefulSpec("v2"))

	waitFor(t, 4*time.Second, "v2 instance started", func() bool {
		return len(base.pidsOf("db-v1-0")) >= 1
	})
	stopped, _ := slow.times("db-0")
	_, started := slow.times("db-v1-0")
	if stopped.IsZero() || started.IsZero() || !started.After(stopped) {
		t.Fatalf("new writer started at %v but the old one's Stop returned at %v", started, stopped)
	}
	waitFor(t, 4*time.Second, "v2 ready and routed alone", func() bool {
		return a.AllReady() && px.has("db", "exec://db-v1-0") && !px.has("db", "exec://db-0")
	})
}

func TestStatefulReconfigureDuringStopTakesTheLatestSpec(t *testing.T) {
	base := newExecLauncher()
	slow := &slowStopLauncher{Launcher: base, delay: 400 * time.Millisecond, stopReturn: map[string]time.Time{}, startAt: map[string]time.Time{}}
	rc := newFakeChecker(250 * time.Millisecond)
	rc.set("db-0", "pass")
	rc.set("db-v1-0", "pass")

	px := newFakeProxy()
	a := NewAgent(slow, rc, []Service{statefulSpec("v1")}, proxyOpts(nil, px, time.Minute))
	cancel, wg := startAgent(t, a)
	defer wg.Wait()
	defer cancel()

	waitFor(t, 3*time.Second, "v1 ready", func() bool { return a.AllReady() })

	a.Reconfigure(statefulSpec("v2"))
	time.Sleep(100 * time.Millisecond) // the old writer's Stop is in flight
	a.Reconfigure(statefulSpec("v3"))

	waitFor(t, 5*time.Second, "one new instance started", func() bool {
		return len(base.pidsOf("db-v1-0")) >= 1
	})
	stopped, _ := slow.times("db-0")
	_, started := slow.times("db-v1-0")
	if !started.After(stopped) {
		t.Fatalf("new writer started at %v before the old one's Stop returned at %v", started, stopped)
	}
	waitFor(t, 5*time.Second, "ready on the latest spec", func() bool {
		if !a.AllReady() {
			return false
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.services["db"].spec.Image == "v3"
	})
	if n := len(base.pidsOf("db-v1-0")) + len(base.pidsOf("db-v2-0")); n != 1 {
		t.Fatalf("expected exactly one writer, found %d", n)
	}
}
