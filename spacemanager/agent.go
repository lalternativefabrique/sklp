package spacemanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/lalternativefabrique/sklp/ingress"
	"github.com/lalternativefabrique/sklp/space"
)

// Default backoff bounds. Exposed via Options for tests (short delays).
const (
	defaultBackoffBase = 1 * time.Second
	defaultBackoffMax  = 30 * time.Second
	// stableAfter is how long an instance must live before its backoff is
	// reset to base. A service that crashes immediately keeps escalating;
	// one that ran a while is treated as a fresh, isolated failure.
	defaultStableAfter = 10 * time.Second
	// rolloutTimeout caps how long a rolling update waits for the new instance
	// to become Ready before declaring the rollout Failed and keeping the old
	// instance (rollback-on-failure). Overridable via Options for tests.
	defaultRolloutTimeout = 2 * time.Minute
	// jobMaxRetries caps how many times a failing oneshot job (exit != 0) is
	// relaunched with backoff before it is HELD Failed (fail-closed): a
	// migration that cannot apply must never let its dependents boot. After the
	// cap the job stops relaunching and stays Failed so AllReady stays false.
	// Sized like the backoff escalation (a handful of capped attempts).
	jobMaxRetries = 5
)

// deathSettleTimeout bounds how long a job death waits for its exit code: a
// process that died before any reaper saw it never yields one, and waiting
// forever held the job "in flight" and skipped every later scheduled run.
var deathSettleTimeout = 30 * time.Second

// instance is one running (or scheduled-to-restart) replica of a service. Each
// instance carries its own pid/pidfd/state/backoff (milestones 1-2 applied per
// instance), keyed in the agent by its instance id (<service>-<i>).
type instance struct {
	id        string    // <service>-<i>, the launcher/proxy identity
	svcName   string    // owning service name
	svc       Service   // the spec this instance was launched from (svc.Name = id)
	state     State     // observable lifecycle of the current run
	pid       int       // init PID of the current run; 0 when not running
	launching bool      // a launch() is in flight: Start ran lock-free, pid not set yet
	startedAt time.Time // when the current run was launched
	// runTrigger names what asked for the current run of a job, so its history
	// tells an operator-clicked run from a scheduled or image-rolled one.
	runTrigger string
	bo         backoff   // per-instance restart backoff
	restartAt  time.Time // when to (re)launch; zero = launch asap

	// jobFails counts non-zero exits of a oneshot job. Once it reaches
	// jobMaxRetries the job is HELD Failed (fail-closed): never relaunched, so
	// dependents stay Pending and AllReady stays false. Unused for services.
	jobFails int

	// exitAwaitedSince is when onDeath first re-deferred a death whose exit
	// code was not harvested yet; zero once the death settles.
	exitAwaitedSince time.Time

	// bounced marks a teardown this agent asked for, so the death it causes
	// is not counted against the service.
	bounced bool

	// crashes counts the deaths this instance did not ask for: a service that
	// died on its own, or a job that exited non-zero. A job exiting 0 is the
	// job succeeding, not a symptom. Counting launches instead would be wrong
	// twice over — a stop races its own relaunch, so one operator bounce can
	// produce two launches, and a bounce is not a symptom anyway.
	// gen is a race token, not a tally.
	crashes int

	// gen identifies the current run. Every launch bumps it; a readiness
	// goroutine captures the gen at spawn and only applies its result if gen
	// still matches under the lock. This is the guard against the death-during-
	// readiness race: a death (or relaunch) bumps gen, so a late WaitReady
	// return for the previous run is discarded instead of marking a dead/older
	// run Ready.
	gen int
	// readyCancel cancels the in-flight readiness wait for the current run.
	// Called on death and teardown so a dying container aborts its own gate.
	readyCancel context.CancelFunc

	// retired marks an old-generation instance that a rolling update is in the
	// process of replacing. A retired instance is no longer restarted on death
	// (the rollout owns its lifecycle) and is excluded from AllReady.
	retired bool
	stopped bool // Stop already issued by the stateful rollout; skip it at cutover
}

// restarts is how many times the instance died on its own and came back.
func (inst *instance) restarts() int { return inst.crashes }

// serviceState groups the instances of one service and the in-flight rolling
// update, if any. Reconciliation targets "replicaCount instances Ready" per
// service; a spec change triggers a start-first rolling replacement.
type serviceState struct {
	spec Service // the runnable spec the steady-state instances should match

	// Rolling update state. When rolling, newSpec is the target definition and
	// newIDs are the instance ids started for it; the old (pre-change)
	// instances stay alive and in the proxy until the new ones are Ready.
	rolling   bool
	newSpec   Service
	newIDs    []string  // instance ids started for newSpec
	oldIDs    []string  // instance ids being replaced (kept until cutover)
	rolloutAt time.Time // deadline for the in-flight rollout
	// stopping counts old instances whose Stop has not returned: a service
	// with a volume has one writer, so its new instance waits for them.
	stopping   int
	stoppedOld bool
	// soaking is the window after cutover during which the old generation is
	// de-routed but still alive, so a crash of the new one is undone by routing
	// back. newCrashes counts the new generation's deaths before its first Ready.
	soaking      bool
	soakAt       time.Time
	newCrashes   int
	rolloutID    string
	rolloutStart time.Time
	// rejected is the last spec whose rollout was rolled back; re-applying it
	// verbatim is refused until a different spec arrives or the service is
	// restarted, or every reconcile would replay the same failure.
	rejected *Service
	// pending is a spec that arrived during the soak; it is applied once the
	// rollout settles.
	pending  *Service
	rollouts []RolloutRecord
	// runs is this job's recent executions, oldest first. A job's terminal
	// state carries no clock, and the container log is truncated on every
	// launch, so without this an operator cannot tell a run that just happened
	// from the one before it — least of all when both print the same line.
	runs []JobRunRecord

	// liveIDs is the current steady-state instance ids of the service. Empty
	// means "use the canonical <svc>-0..N-1 slots" (the initial generation);
	// after a rolling update it holds the adopted "-v-" ids so reconcile keeps
	// them running instead of spawning duplicate canonical slots.
	liveIDs []string

	// rolloutGen counts completed rollouts of this service. It is folded into
	// the next rollout's instance ids (<svc>-v<rolloutGen>-<i>) so a SECOND (or
	// later) rollout never reuses the ids the previous one promoted into
	// liveIDs — reusing them would clobber the live, Ready instances' map
	// entries in a.instances before the new generation is even checked for
	// readiness, silently destroying the start-first safety property.
	rolloutGen int

	// backends is the last address set pushed to the proxy for this service,
	// kept so we only call SetBackends when the Ready set actually changes.
	backends []string
}

// slotIDs is the set of instance ids the steady-state service should run: the
// adopted live ids if a rollout promoted some, else the canonical slots.
func (st *serviceState) slotIDs() []string {
	if len(st.liveIDs) > 0 {
		return append([]string(nil), st.liveIDs...)
	}
	ids := make([]string, 0, st.spec.replicaCount())
	for i := 0; i < st.spec.replicaCount(); i++ {
		ids = append(ids, instanceID(st.spec.Name, i))
	}
	return ids
}

// Options tunes the agent. Zero values fall back to the defaults; tests pass
// short delays to keep runs fast.
type Options struct {
	BackoffBase    time.Duration
	BackoffMax     time.Duration
	StableAfter    time.Duration
	RolloutTimeout time.Duration
	// Soak is how long a new generation must stay Ready after cutover before
	// the previous one is stopped; a service's ready.soak overrides it.
	Soak time.Duration
	// ProbeInterval is how often a Ready instance is re-probed; ProbeFailures
	// consecutive failures degrade it (out of the proxy, not restarted) and
	// one success re-admits it.
	ProbeInterval time.Duration
	ProbeFailures int
	Logger        *log.Logger
	// OnRollout, if set, receives every finished rollout record (off the lock).
	OnRollout func(RolloutRecord)
	// Observer, if set, receives every state transition. The mechanism a
	// future reporter (terminal / Skalpai UI) consumes; nil = log-only.
	Observer StateObserver
	// Proxy, if set, fronts the proxied services (those with Port). The agent
	// reconfigures it whenever a service's Ready instance set changes. nil = no
	// proxy (single-instance, no-routing behaviour of milestones 1-2).
	Proxy proxyController
	// Gateway, if set, is the host-level front door for DOMAINED services (those
	// with a non-empty Domain). The agent declares/refreshes a route whenever a
	// domained service's Ready backend set changes and Unroutes on removal. nil =
	// no gateway (the route is never declared; the multi-space sibling of Proxy).
	Gateway Gateway
	// Space is the name of the space these services belong to. It carries into
	// every Domain the agent routes (Domain.Space) so the gateway can disambiguate
	// the same service name across spaces. Empty when no gateway is wired.
	Space string
	// Routes are the resolved public gateway routes of this space. The agent
	// groups them by service and declares each service's route set when its
	// backends move. Empty when the space exposes nothing through the gateway.
	Routes []ResolvedRoute
	// MeshResolver, if set, resolves a mesh route target ("<space>/<service>")
	// to the mesh IP of the node hosting it (the netctrl peer cache behind a
	// narrow func, so the agent stays free of mesh types). nil = mesh routes
	// never resolve, and stay unrouted.
	MeshResolver func(space, service string) (meshIP string, ok bool)
	// LogSink, if set AND the launcher exposes per-instance log paths, receives
	// every container stdout/stderr line tagged by service name (docker-compose
	// style). nil = container logs are not streamed (test default).
	LogSink LogSink
	// OnAllReady, if set, is called EXACTLY ONCE the first time every desired
	// service has its full replica count Ready — the cue a front-end uses to
	// print the "now supervising; Ctrl-C to tear down" banner before Run goes
	// quiet waiting on ctx. Never called for a set that never fully comes up.
	OnAllReady func()
	// Watcher, if set, replaces the default pidfdWatcher. Use a fakeDeathWatcher
	// in tests to drive death events without real processes.
	Watcher DeathWatcher
	// GatewayMode is this space's front-door mode (singleton | per-space), as
	// resolved by the NODE (never by a deploying app's deploy.yaml — see
	// DeployConfig.Resolve). Exposed back via Agent.GatewayMode so the control
	// server can assert its own authoritative value against an incoming remote
	// apply instead of trusting the client's.
	GatewayMode string
}

// Agent is the single-node reconciliation core: it launches the desired
// services (N instances each), watches every instance via pidfd, reaps dead
// inits (subreaper), relaunches them with capped backoff, keeps the reverse
// proxy pointed at the Ready instances, and rolls services forward
// zero-downtime on a spec change. One Agent owns one set of services for the
// lifetime of Run.
type Agent struct {
	launcher    Launcher
	watcher     DeathWatcher
	wakeCh      chan struct{} // internal wake signal for eventLoop
	ready       readyChecker
	proxy       proxyController
	gateway     Gateway                                    // host-level front door for domained services; nil when unwired
	meshResolve func(space, service string) (string, bool) // mesh route target -> hosting node's mesh IP; nil = never resolves
	meshRouted  map[string]string                          // mesh route key -> last declared backend+routes stamp
	ingress     ingressController                          // host->space port ingress; nil when unsupported
	gwback      gatewayBackender                           // host-reachable backend for domained services; nil when unsupported
	l7          l7Router                                   // native in-worker L7 ingress route push; nil when unsupported
	ipPort      ipPortProvider                             // per-instance container IP+port; nil when unsupported
	acl         aclApplier                                 // east-west forward-chain ACL push; nil when unsupported
	opts        Options
	logger      *log.Logger
	observe     StateObserver
	logs        *logStreamer // streams container stdout/stderr; no-op when unwired

	// mappings is the parsed host:container port set per service, derived once
	// from Service.Ports at construction. ingressUp guards OpenIngress to a
	// single call per service (the first time it reaches Ready).
	mappings  map[string][]PortMapping
	ingressUp map[string]bool

	// routesBySvc groups the space's gateway routes by service name, derived once
	// from Options.Routes at construction. gatewayRouteLocked writes a service's
	// whole route set as one vendor file when its backends move. Empty for a
	// service with no public route.
	routesBySvc map[string][]ResolvedRoute

	mu        sync.Mutex
	services  map[string]*serviceState // service name -> grouping + rollout
	order     []string                 // service names in desired order (stable iteration)
	instances map[string]*instance     // instance id -> instance

	readyWG sync.WaitGroup // tracks in-flight readiness goroutines

	allReadyFired bool // guards OnAllReady to a single call (under mu)
}

// NewAgent builds an agent for a fixed desired set, an injected launcher, and
// an injected readiness checker. A nil checker means no service gates on a
// probe (milestone-1 behaviour); pass a real one to honour Service.Ready. The
// proxy (Options.Proxy) is injected the same way; nil = no routing.
// Set Options.Watcher to inject a custom DeathWatcher (e.g. fakeDeathWatcher in
// tests); nil means Run creates a pidfdWatcher automatically.
func NewAgent(launcher Launcher, ready readyChecker, desired []Service, opts Options) *Agent {
	if opts.BackoffBase == 0 {
		opts.BackoffBase = defaultBackoffBase
	}
	if opts.BackoffMax == 0 {
		opts.BackoffMax = defaultBackoffMax
	}
	if opts.StableAfter == 0 {
		opts.StableAfter = defaultStableAfter
	}
	if opts.RolloutTimeout == 0 {
		opts.RolloutTimeout = defaultRolloutTimeout
	}
	if opts.Soak == 0 {
		opts.Soak = defaultSoak
	}
	if opts.ProbeInterval == 0 {
		opts.ProbeInterval = defaultProbeInterval
	}
	if opts.ProbeFailures == 0 {
		opts.ProbeFailures = defaultProbeFailures
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.New(os.Stderr, "[sklp deploy] ", log.LstdFlags)
	}
	a := &Agent{
		launcher:    launcher,
		watcher:     opts.Watcher,
		wakeCh:      make(chan struct{}, 1),
		ready:       ready,
		proxy:       opts.Proxy,
		gateway:     opts.Gateway,
		meshResolve: opts.MeshResolver,
		opts:        opts,
		logger:      logger,
		observe:     opts.Observer,
		services:    make(map[string]*serviceState),
		instances:   make(map[string]*instance),
		mappings:    make(map[string][]PortMapping),
		ingressUp:   make(map[string]bool),
		routesBySvc: groupRoutesBySvc(opts.Routes),
	}
	// Host ingress is active only when the launcher supports it (runc + space
	// net). The exec test launcher does not implement it, so this stays nil and
	// OpenIngress is never called.
	if ic, ok := launcher.(ingressController); ok {
		a.ingress = ic
	}
	// Gateway-backend resolution is active only when the launcher can bridge a
	// domained service into the space netns (runc + space net). The exec test
	// launcher does not implement it, so this stays nil and gatewayBackendsLocked
	// returns nil for every service.
	if gb, ok := launcher.(gatewayBackender); ok {
		a.gwback = gb
	}
	// Native in-worker L7 ingress is active only when the launcher can push routes
	// to the space worker (runc + space net). The exec test launcher does not
	// implement it, so this stays nil and pushL7RoutesLocked is a no-op.
	// IP/port observability for GET /v1/state is active only when the launcher
	// can resolve a real per-instance address (runc + space net). The exec test
	// launcher does not implement it, so Snapshot's IP/Port stay zero for it —
	// the Network Controller (spacemanager/netctrl) is the intended reader of
	// this, polling read-only rather than being pushed to by Space.
	if ip, ok := launcher.(ipPortProvider); ok {
		a.ipPort = ip
	}
	if r, ok := launcher.(l7Router); ok {
		a.l7 = r
	}
	// East-west ACL push is active only when the launcher can reach the space
	// worker's acl command (runc + space net). The exec test launcher does not
	// implement it, so this stays nil and syncACLLocked is a no-op — an
	// unsupported launcher's Space keeps the flat east-west it always had.
	if ap, ok := launcher.(aclApplier); ok {
		a.acl = ap
	}
	// Container-log streaming is active only when a sink is configured AND the
	// launcher can map an instance id to its captured log file. The exec test
	// launcher implements no LogPath, so this stays a no-op there.
	if provider, ok := launcher.(logPathProvider); ok {
		a.logs = newLogStreamer(opts.LogSink, provider)
	} else {
		a.logs = newLogStreamer(opts.LogSink, nil)
	}
	// Reconcile in dependency order: every name comes after its upstreams so a
	// gated dependent is only ever considered once its upstreams have been
	// driven this tick. The order is topo-sorted over the union (services+jobs)
	// graph, ties broken lexicographically for determinism. A cycle/unknown ref
	// is already rejected at config load (Layer 1); here we fall back to the
	// insertion order so a directly-built agent never panics.
	order := topoOrder(desired)
	for _, name := range order {
		svc := byNameLocal(desired, name)
		a.services[svc.Name] = &serviceState{spec: svc}
		a.order = append(a.order, svc.Name)
		// Parse the host:container port mappings once. A malformed entry is
		// already rejected at config load; re-validate here so a directly-built
		// agent (tests, future callers) skips a bad mapping with a clear log
		// rather than panicking or exposing nothing silently.
		if maps, err := servicePortMappings(svc); err != nil {
			logger.Printf("service %q: ignoring port mappings: %v", svc.Name, err)
		} else if len(maps) > 0 {
			a.mappings[svc.Name] = maps
		}
	}
	return a
}

// byNameLocal returns the desired spec named name (it always exists; topoOrder
// is built from the same set).
func byNameLocal(desired []Service, name string) Service {
	for _, s := range desired {
		if s.Name == name {
			return s
		}
	}
	return Service{Name: name}
}

// topoOrder returns the desired names topologically sorted over the union
// DependsOn graph so each name follows all of its upstreams; independent names
// keep a deterministic lexicographic order. Ported from runtime.orderServices
// (same DFS) rather than cross-imported, to keep the deploy layer self-contained.
// A cycle or an unknown dependency falls back to the insertion order — those are
// rejected upstream at config load, so this only guards a hand-built agent.
func topoOrder(desired []Service) []string {
	byName := make(map[string]Service, len(desired))
	for _, s := range desired {
		byName[s.Name] = s
	}
	names := make([]string, 0, len(desired))
	for _, s := range desired {
		names = append(names, s.Name)
	}
	sort.Strings(names)

	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(desired))
	var ordered []string
	var bad bool
	var visit func(name string)
	visit = func(name string) {
		switch color[name] {
		case black:
			return
		case gray:
			bad = true // cycle; bail to insertion order
			return
		}
		if _, ok := byName[name]; !ok {
			bad = true // unknown dep
			return
		}
		color[name] = gray
		deps := append([]string(nil), byName[name].DependsOn...)
		sort.Strings(deps)
		for _, dep := range deps {
			// A "space/service" target (ADR-007) names another Space's own
			// service — this Space's topo order has no business over it, and
			// it is never a member of byName, so visiting it would always
			// hit the "unknown dep" fallback below and degrade this WHOLE
			// graph's order for services that have nothing to do with it.
			if strings.Contains(dep, "/") {
				continue
			}
			visit(dep)
		}
		color[name] = black
		ordered = append(ordered, name)
	}
	for _, name := range names {
		visit(name)
	}
	if bad || len(ordered) != len(desired) {
		out := make([]string, 0, len(desired))
		for _, s := range desired {
			out = append(out, s.Name)
		}
		return out
	}
	return ordered
}

// instanceID is the stable id of replica i of a service.
func instanceID(service string, i int) string {
	return fmt.Sprintf("%s-%d", service, i)
}

// maybeFireAllReadyLocked fires OnAllReady the first time the whole desired set
// is Ready, then never again. Caller holds a.mu; the callback runs in a fresh
// goroutine so a slow front-end never stalls the reconcile path under the lock.
func (a *Agent) maybeFireAllReadyLocked() {
	if a.allReadyFired || a.opts.OnAllReady == nil {
		return
	}
	if !a.allReadyLocked() {
		return
	}
	a.allReadyFired = true
	go a.opts.OnAllReady()
}

// transition records a state change on inst, logs it, and emits it to the
// observer keyed by the OWNING SERVICE (the observable unit a reporter renders;
// the instance id appears in the detail). Caller MUST hold a.mu. A no-op when
// the state is unchanged.
func (a *Agent) transition(inst *instance, to State, detail string) {
	from := inst.state
	if from == to {
		return
	}
	inst.state = to
	d := detail
	if d != "" {
		a.logger.Printf("instance %q: %s -> %s (%s)", inst.id, from, to, d)
	} else {
		a.logger.Printf("instance %q: %s -> %s", inst.id, from, to)
	}
	if a.observe != nil {
		a.observe(StateEvent{Service: inst.svcName, From: from, To: to, Detail: detailWithInstance(inst.id, d)})
	}
}

func detailWithInstance(id, detail string) string {
	if detail == "" {
		return id
	}
	return id + ": " + detail
}

// Run reconciles the desired set until ctx is cancelled, then tears every
// managed container down. If no DeathWatcher was provided at construction a
// pidfdWatcher is created here (production path). Tests pass a fakeDeathWatcher
// to drive deaths without real processes.
func (a *Agent) Run(ctx context.Context) error {
	if a.watcher == nil {
		pw := newPidfdWatcher(a.logger)
		if err := pw.Start(ctx); err != nil {
			return err
		}
		a.watcher = pw
	}
	defer a.watcher.Close()
	defer a.teardown()

	// Consume deaths from the watcher and drive reconcile on a ticker for
	// scheduled restarts and rollout deadlines. teardown must not start while
	// a reconcile is still launching: it resets the instances that launch is
	// writing to, and the launched process would then outlive the agent.
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		a.eventLoop(ctx)
	}()

	a.reconcile(ctx)
	<-ctx.Done()
	<-loopDone
	return ctx.Err()
}

// eventLoop drains the watcher's Deaths channel and re-runs reconcile on every
// event or on the next scheduled deadline (restart backoff / rollout timeout).
// The deadline timer is recomputed after every reconcile so a newly-scheduled
// restart or rollout deadline is picked up on the next iteration.
func (a *Agent) eventLoop(ctx context.Context) {
	for {
		timer := a.nextDeadlineTimer(ctx)
		select {
		case <-ctx.Done():
			return
		case id := <-a.watcher.Deaths():
			a.onDeath(id)
			a.reconcile(ctx)
		case <-a.wakeCh:
			a.reconcile(ctx)
		case <-timer:
			a.reconcile(ctx)
		}
	}
}

// nextDeadlineTimer returns a channel that fires at the earliest scheduled
// restart or rollout deadline, or blocks forever when nothing is pending.
func (a *Agent) nextDeadlineTimer(ctx context.Context) <-chan time.Time {
	a.mu.Lock()
	var next time.Time
	consider := func(t time.Time) {
		if t.IsZero() {
			return
		}
		if next.IsZero() || t.Before(next) {
			next = t
		}
	}
	for _, inst := range a.instances {
		if inst.pid == 0 {
			consider(inst.restartAt)
		}
	}
	for _, st := range a.services {
		if st.rolling {
			consider(st.rolloutAt)
			consider(st.soakAt)
		}
	}
	a.mu.Unlock()

	if next.IsZero() {
		return make(chan time.Time) // block forever
	}
	d := time.Until(next)
	if d <= 0 {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	return time.After(d)
}

// onDeath records that instance `id` died: pull it from the proxy backends (a
// dead instance must never be a routing target), close its pidfd, resolve the
// exit code, reset backoff if it ran long enough, and schedule a restart. A
// retired instance (being replaced by a rollout) is NOT restarted.
func (a *Agent) onDeath(id string) {
	a.mu.Lock()
	inst := a.instances[id]
	if inst == nil || inst.pid == 0 {
		a.mu.Unlock()
		return
	}
	ran := time.Since(inst.startedAt)
	wasReady := inst.state == Ready
	if inst.readyCancel != nil {
		inst.readyCancel()
		inst.readyCancel = nil
	}
	deadPID := inst.pid
	inst.pid = 0
	inst.gen++
	a.logs.stop(id)
	a.watcher.Unwatch(id)

	svcName := inst.svcName
	retired := inst.retired

	if retired {
		delete(a.instances, id)
		a.transition(inst, Failed, "process exited (retired)")
		code, known := a.watcher.ExitCode(deadPID)
		dropReaped(deadPID)
		if wasReady {
			a.syncProxyLocked(svcName)
			a.gatewayRouteLocked(svcName)
			a.syncACLLocked()
		}
		a.mu.Unlock()
		a.launcher.Stop(id)
		a.logExit(id, code, known, ran, 0)
		return
	}

	if st := a.services[svcName]; st != nil && st.rolling && !inst.svc.Oneshot && st.isNewID(id) {
		code, known := a.watcher.ExitCode(deadPID)
		if !known {
			code, known = a.watcher.ReapNow(deadPID)
		}
		exit := "unknown exit"
		if known {
			exit = "exit " + strconv.Itoa(code)
		}
		reason := ""
		switch {
		case st.soaking:
			reason = fmt.Sprintf("%s exited (%s) %v after cutover, inside the soak window", id, exit, ran.Round(time.Millisecond))
		default:
			st.newCrashes++
			if st.newCrashes >= rolloutCrashLimit {
				reason = fmt.Sprintf("%s exited (%s) %d times before becoming Ready", id, exit, st.newCrashes)
			}
		}
		if reason != "" {
			dropReaped(deadPID)
			a.abortRolloutLocked(st, RolloutRolledBack, reason, code)
			a.mu.Unlock()
			a.logExit(id, code, known, ran, 0)
			a.wake()
			return
		}
	}

	// A job's exit is expected, so only a long-lived service dying counts.
	// Charged here rather than on entry: a re-deferred death (the exit code
	// was not harvested yet) re-enters onDeath and would be counted twice.
	if !inst.svc.Oneshot {
		if inst.bounced {
			inst.bounced = false
		} else {
			inst.crashes++
		}
	}

	// Job-aware death path: a oneshot job's exit is EXPECTED, not a crash.
	if inst.svc.Oneshot {
		code, known := a.watcher.ExitCode(deadPID)
		if !known {
			// Watcher may not have harvested this pid yet — reap directly.
			code, known = a.watcher.ReapNow(deadPID)
		}
		if !known && inst.exitAwaitedSince.IsZero() {
			inst.exitAwaitedSince = time.Now()
		}
		if !known && time.Since(inst.exitAwaitedSince) > deathSettleTimeout {
			a.logger.Printf("job %q: exit code of pid %d never harvested after %v; counting the run as failed", id, deadPID, deathSettleTimeout)
			code = -1
		} else if !known {
			// The pidfd confirmed the death but the exit code is not yet
			// harvested (the async SIGCHLD reaper raced and we lost). Do NOT guess
			// success or failure — RE-DEFER the death: keep the instance, restore
			// its pid so onDeath re-runs, and re-enqueue it. Next round drainReap
			// will have stored the code so we decide correctly (the flaky
			// double-relaunch came from presuming failure here).
			inst.pid = deadPID
			inst.gen--
			a.mu.Unlock()
			a.watcher.requeueDeath(id, deadPID)
			return
		}
		inst.exitAwaitedSince = time.Time{}
		if known && code == 0 {
			dropReaped(deadPID)
			inst.restartAt = time.Time{}
			a.recordJobRunLocked(inst, code, false)
			a.transition(inst, Done, "job completed (exit 0)")
			a.retryRejectedDependentsLocked(svcName)
			a.maybeFireAllReadyLocked()
			a.mu.Unlock()
			a.launcher.Stop(id)
			a.logger.Printf("job %q completed after %v", id, ran.Round(time.Millisecond))
			a.watcher.Wake()
			return
		}
		dropReaped(deadPID)
		inst.jobFails++
		inst.crashes++
		a.recordJobRunLocked(inst, code, true)
		if inst.jobFails >= jobMaxRetries {
			inst.restartAt = time.Time{}
			a.transition(inst, Failed, fmt.Sprintf("job failed (exit %d); giving up after %d attempts", code, inst.jobFails))
			a.mu.Unlock()
			a.launcher.Stop(id)
			a.logger.Printf("job %q failed permanently after %d attempts (exit %d)", id, inst.jobFails, code)
			return
		}
		delay := inst.bo.next()
		inst.restartAt = time.Now().Add(delay)
		a.transition(inst, Failed, "job exited non-zero")
		a.transition(inst, Restarting, "retry in "+delay.String())
		a.mu.Unlock()
		a.launcher.Stop(id)
		a.logExit(id, code, known, ran, delay)
		return
	}

	if ran >= a.opts.StableAfter {
		inst.bo.reset()
	}
	delay := inst.bo.next()
	inst.restartAt = time.Now().Add(delay)
	a.transition(inst, Failed, "process exited")
	a.transition(inst, Restarting, "restart in "+delay.String())
	code, known := a.watcher.ExitCode(deadPID)
	dropReaped(deadPID)
	if wasReady {
		a.syncProxyLocked(svcName)
		a.gatewayRouteLocked(svcName)
		a.syncACLLocked()
	}
	a.mu.Unlock()

	a.launcher.Stop(id)
	a.logExit(id, code, known, ran, delay)
}

func (a *Agent) logExit(id string, code int, known bool, ran, delay time.Duration) {
	if delay == 0 {
		if known {
			a.logger.Printf("instance %q exited (code %d) after %v (retired)", id, code, ran.Round(time.Millisecond))
		} else {
			a.logger.Printf("instance %q exited after %v (retired)", id, ran.Round(time.Millisecond))
		}
		return
	}
	if known {
		a.logger.Printf("instance %q exited (code %d) after %v, restarting in %v", id, code, ran.Round(time.Millisecond), delay)
	} else {
		a.logger.Printf("instance %q exited after %v, restarting in %v", id, ran.Round(time.Millisecond), delay)
	}
}

// reconcile drives actual toward desired for every service: maintain the right
// number of instances at the right spec, (re)launch any not running whose
// restart deadline passed, and advance any in-flight rolling update.
func (a *Agent) reconcile(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	now := time.Now()
	// Snapshot order under the lock: RemoveService mutates a.order/a.services
	// concurrently, so ranging the live slice (and trusting the name still
	// resolves) races and can hand reconcile a nil serviceState.
	a.mu.Lock()
	order := append([]string(nil), a.order...)
	a.mu.Unlock()
	for _, name := range order {
		a.mu.Lock()
		st := a.services[name]
		if st == nil {
			// Removed between the snapshot and this lock — nothing to reconcile.
			a.mu.Unlock()
			continue
		}
		// Ensure the steady-state instance records exist for the live slots.
		slots := st.slotIDs()
		for _, id := range slots {
			if a.instances[id] == nil {
				a.instances[id] = a.newInstanceLocked(name, st.spec, id)
			}
		}
		// Scale DOWN: stop+forget any steady-state instance of this service whose
		// slot is no longer desired (Replicas was lowered). Without this the
		// surplus containers keep running AND stay registered in the space DNS, so
		// the gateway/ingress keeps load-balancing onto replicas that should be
		// gone. Collect the surplus ids under the lock; stop them outside it.
		var toStop []string
		if !st.rolling { // a rollout manages its own old-generation teardown
			want := make(map[string]bool, len(slots))
			for _, id := range slots {
				want[id] = true
			}
			for id, inst := range a.instances {
				if inst == nil || inst.svcName != name || inst.retired {
					continue
				}
				if !want[id] {
					inst.retired = true // stop restarting it on death
					toStop = append(toStop, id)
				}
			}
		}
		// Gate the whole service/job on its upstreams: a dependent is launched
		// only once every upstream is satisfied (service Ready / job Done). The
		// reconcile order is topo-sorted, so upstreams were already considered
		// this tick. depsOK is read once; instances stay Pending while false.
		depsOK := a.dependenciesSatisfiedLocked(name)
		// Collect launch work for the live (non-retired) instances of this
		// service while holding the lock; launch outside it.
		var toLaunch []Service
		for _, id := range slots {
			inst := a.instances[id]
			if inst == nil || inst.retired {
				continue
			}
			// A oneshot job in a terminal state (Done) or held Failed after the
			// retry cap is never (re)launched. A Failed/Restarting job under the
			// cap still retries, so only Done is terminal here.
			if inst.svc.Oneshot && (inst.state == Done || a.jobHeldFailedLocked(inst)) {
				continue
			}
			if depsOK && inst.pid == 0 && !inst.launching && !now.Before(inst.restartAt) {
				// Reserve the launch UNDER THE LOCK: Start() runs lock-free in
				// launch(), so without this a later reconcile tick would see pid==0
				// again and double-launch a fast-exiting oneshot (the flaky
				// TestJob_CleanExitNotCrashLooped). launch() clears it once pid is set.
				inst.launching = true
				toLaunch = append(toLaunch, inst.svc)
			}
		}
		// During a rollout, also drive the new-generation instances. A oneshot
		// job never reaches Ready (see below) — once Done, its rollout copy
		// must stop relaunching exactly like the steady-state guard above, or
		// it re-runs forever once pid resets to 0 after each completed run.
		if st.rolling {
			for _, id := range st.newIDs {
				inst := a.instances[id]
				if inst == nil || inst.retired {
					continue
				}
				if inst.svc.Oneshot && (inst.state == Done || a.jobHeldFailedLocked(inst)) {
					continue
				}
				if inst.pid == 0 && !inst.launching && !now.Before(inst.restartAt) {
					inst.launching = true
					toLaunch = append(toLaunch, inst.svc)
				}
			}
		}
		rolling := st.rolling
		a.mu.Unlock()

		// Tear down the scaled-away surplus: stop each container (which detaches it
		// from the bridge and unregisters its IP from the space DNS) and forget the
		// instance, then refresh the front door so it stops routing to them.
		if len(toStop) > 0 {
			for _, id := range toStop {
				a.launcher.Stop(id)
			}
			a.mu.Lock()
			for _, id := range toStop {
				delete(a.instances, id)
			}
			a.syncProxyLocked(name)
			a.gatewayRouteLocked(name)
			a.syncACLLocked()
			a.mu.Unlock()
		}

		for _, svc := range toLaunch {
			a.launch(ctx, svc)
		}

		if rolling {
			a.advanceRollout(ctx, name)
		}
	}
}

// dependenciesSatisfiedLocked reports whether every upstream of name is
// satisfied: an upstream service has its required replicas Ready, an upstream
// job is Done. The DependsOn names are already resolved (Layer 1 rewrote them
// under the ephemeral prefix), so they index a.services directly. A
// "space/service" target (ADR-007) names a service in ANOTHER Space's own
// deploy — this Space's agent has no visibility into that Space's readiness,
// and ordering was never the point of a cross-Space depends_on: it exists
// solely as the same-host relay's ACL source, checked by the calling
// worker at connection time, not here. So it is skipped for ordering,
// unconditionally satisfied. An unknown SAME-space upstream (should not
// happen post-validation) is treated as unsatisfied — fail-closed. Caller
// holds a.mu.
func (a *Agent) dependenciesSatisfiedLocked(name string) bool {
	st := a.services[name]
	if st == nil {
		return false
	}
	return a.upstreamsSatisfiedLocked(st.spec.DependsOn)
}

func (a *Agent) upstreamsSatisfiedLocked(deps []string) bool {
	for _, dep := range deps {
		if strings.Contains(dep, "/") {
			continue
		}
		up := a.services[dep]
		if up == nil {
			return false
		}
		if up.spec.Oneshot {
			if !a.jobDoneLocked(dep) {
				return false
			}
			continue
		}
		// Upstream service: all required replicas Ready.
		insts := a.liveInstancesLocked(dep)
		if len(insts) < up.spec.replicaCount() {
			return false
		}
		for _, inst := range insts {
			if inst.state != Ready {
				return false
			}
		}
	}
	return true
}

// jobDoneLocked reports whether the oneshot job named name has reached Done.
// Caller holds a.mu.
func (a *Agent) jobDoneLocked(name string) bool {
	for _, inst := range a.liveInstancesLocked(name) {
		if inst.state == Done {
			return true
		}
	}
	return false
}

// retryRejectedDependentsLocked rolls each service that depends on job again
// to the spec it last rolled back from: a candidate that failed against the
// previous schema gets one more try once the job has run. Caller holds a.mu.
func (a *Agent) retryRejectedDependentsLocked(job string) {
	for _, name := range a.order {
		st := a.services[name]
		if st == nil || st.rejected == nil || st.rolling || !slices.Contains(st.spec.DependsOn, job) {
			continue
		}
		next := *st.rejected
		a.logger.Printf("service %q: job %q completed; retrying the rolled-back image %s", name, job, next.Image)
		a.beginRolloutLocked(st, next)
	}
}

// failedUpstreamJobLocked names the first same-space upstream job held Failed
// after its retry cap, or "" when none is.
func (a *Agent) failedUpstreamJobLocked(deps []string) string {
	for _, dep := range deps {
		up := a.services[dep]
		if up == nil || !up.spec.Oneshot {
			continue
		}
		for _, inst := range a.liveInstancesLocked(dep) {
			if a.jobHeldFailedLocked(inst) {
				return dep
			}
		}
	}
	return ""
}

// jobHeldFailedLocked reports whether a oneshot instance has exhausted its retry
// cap and is being held Failed (fail-closed). Caller holds a.mu.
func (a *Agent) jobHeldFailedLocked(inst *instance) bool {
	return inst.svc.Oneshot && inst.jobFails >= jobMaxRetries
}

// newInstanceLocked builds a fresh instance record for replica id of a service.
// The launched spec carries the instance id as its Name so the launcher/proxy
// key per instance. Caller holds a.mu.
func (a *Agent) newInstanceLocked(svcName string, spec Service, id string) *instance {
	is := spec
	is.Name = id
	return &instance{
		id:      id,
		svcName: svcName,
		svc:     is,
		state:   Pending,
		bo:      newBackoff(a.opts.BackoffBase, a.opts.BackoffMax),
	}
}

// launch starts one instance and opens its pidfd into the watch set. A launch
// failure schedules a backed-off retry rather than aborting the agent.
func (a *Agent) launch(ctx context.Context, svc Service) {
	id := svc.Name
	pid, err := a.launcher.Start(ctx, svc)
	if err != nil {
		a.mu.Lock()
		if inst := a.instances[id]; inst != nil {
			inst.launching = false // launch attempt over; reconcile may retry after backoff
			delay := inst.bo.next()
			inst.restartAt = time.Now().Add(delay)
			a.logger.Printf("instance %q failed to start: %v, retrying in %v", id, err, delay)
		}
		a.mu.Unlock()
		a.wake()
		return
	}

	// Record the pid BEFORE arming the watcher. The watcher can fire onDeath the
	// instant Watch returns; if pid were still unset then, onDeath would see
	// inst.pid == 0 and DISCARD a real death (a fast-exiting oneshot's clean
	// exit), leaving the instance wedged in Starting with a dead pid and later
	// double-launched (the observed flaky TestJob_CleanExitNotCrashLooped).
	// Setting pid under the lock first closes that window.
	a.mu.Lock()
	inst := a.instances[id]
	if inst == nil {
		// Instance was removed while starting; undo (no watch armed yet).
		a.mu.Unlock()
		a.launcher.Stop(id)
		return
	}
	inst.launching = false // pid is now recorded; reconcile sees a live instance
	inst.pid = pid
	inst.startedAt = time.Now()
	inst.restartAt = time.Time{}
	inst.gen++
	gen := inst.gen
	svcName := inst.svcName
	a.transition(inst, Starting, "")
	a.mu.Unlock()

	// Arm the watcher now that the pid is visible to onDeath.
	if werr := a.watcher.Watch(id, pid); werr != nil {
		if errors.Is(werr, unix.ESRCH) {
			// The process already exited AND was reaped (a fast oneshot dying
			// before the pidfd opens): no death event can ever fire, yet the
			// process genuinely ran — the reaper holds its exit code. Route it
			// through the normal death path instead of undoing the launch:
			// onDeath resolves the stored code (Done / backed-off retry) and
			// wakes reconcile, exactly as a watched death would.
			a.logger.Printf("instance %q: process %d exited before watch; deferring to death path", id, pid)
			a.watcher.requeueDeath(id, pid)
			return
		}
		// Any other Watch failure undoes the launch: clear pid, schedule a
		// backed-off retry, stop the process, and wake the event loop — its
		// current deadline timer predates this restartAt.
		a.logger.Printf("instance %q: watch(%d) failed: %v", id, pid, werr)
		a.mu.Lock()
		if inst := a.instances[id]; inst != nil {
			inst.pid = 0
			delay := inst.bo.next()
			inst.restartAt = time.Now().Add(delay)
		}
		a.mu.Unlock()
		a.launcher.Stop(id)
		a.wake()
		return
	}

	a.mu.Lock()
	inst = a.instances[id]
	if inst == nil {
		// Removed during Watch: tear the just-watched instance down.
		a.mu.Unlock()
		a.watcher.Unwatch(id)
		a.launcher.Stop(id)
		return
	}
	// The job may have already exited between the Starting transition above and
	// here (a fast oneshot): onDeath ran, set pid=0, and moved it to a terminal
	// state. Do NOT re-transition it back to Running/Ready — that would resurrect
	// a finished instance. Only proceed for an instance still in this run.
	if inst.pid != pid || inst.gen != gen {
		a.mu.Unlock()
		return
	}

	// Begin streaming this instance's container stdout/stderr (docker-compose
	// style). Tailing from launch means a startup that never reaches Ready still
	// shows its log; the tail is stopped on death/teardown.
	a.logs.start(ctx, id, svcName)

	// A oneshot job has no readiness gate: it is Running until its init exits.
	// onDeath turns a clean exit into Done (terminal) and a non-zero exit into a
	// backed-off Failed retry — it must never enter the service Ready path.
	if inst.svc.Oneshot {
		a.transition(inst, Running, "")
		inst.readyCancel = nil
		a.mu.Unlock()
		return
	}

	if inst.svc.Ready == nil || a.ready == nil {
		a.transition(inst, Ready, "no readiness probe")
		inst.readyCancel = nil
		a.syncProxyLocked(svcName)
		a.gatewayRouteLocked(svcName)
		a.syncACLLocked()
		a.maybeOpenIngressLocked(svcName)
		a.maybeFireAllReadyLocked()
		a.mu.Unlock()
		return
	}

	rctx, rcancel := context.WithCancel(ctx)
	inst.readyCancel = rcancel
	probe := inst.svc.Ready
	a.mu.Unlock()

	a.readyWG.Add(1)
	go a.gateReady(rctx, id, svcName, probe, gen)
}

// gateReady waits for the instance to become ready (or fail/cancel) and applies
// the result only if the run generation is still current — the guard against
// the death-during-readiness race. On success: Ready + proxy refresh. On probe
// error/timeout while still alive: Failed + scheduled backed-off restart. A
// cancelled wait (the init died) is silently dropped; onDeath owns the restart.
func (a *Agent) gateReady(ctx context.Context, id, svcName string, probe *Probe, gen int) {
	defer a.readyWG.Done()
	start := time.Now()
	err := a.ready.WaitReady(ctx, id, probe)
	took := time.Since(start).Round(time.Millisecond)

	a.mu.Lock()
	defer a.mu.Unlock()
	inst := a.instances[id]
	if inst == nil || inst.gen != gen || inst.pid == 0 {
		return // stale: died or relaunched while we waited
	}

	if err != nil {
		a.transition(inst, Failed, err.Error())
		a.recycleLocked(inst)
		a.wake()
		return
	}
	a.transition(inst, Ready, "ready after "+took.String())
	// A new Ready instance enters the proxy backend set and, for a domained
	// service, refreshes the host-gateway route to the current backends.
	a.syncProxyLocked(svcName)
	a.gatewayRouteLocked(svcName)
	a.syncACLLocked()
	a.maybeOpenIngressLocked(svcName)
	a.maybeFireAllReadyLocked()
	a.wake() // a rollout may be waiting for this Ready to cut over

	mctx, mcancel := context.WithCancel(ctx)
	inst.readyCancel = mcancel
	a.readyWG.Add(1)
	go a.monitorReady(mctx, id, svcName, probe, gen)
}

// monitorReady re-runs the probe on a Ready instance for as long as it lives.
// The probe passing at boot only proves the process came up; a service whose
// contract breaks afterwards (a config reload, a dependency gone) stays alive,
// so without this loop it would stay Ready and routed while serving errors.
func (a *Agent) monitorReady(ctx context.Context, id, svcName string, probe *Probe, gen int) {
	defer a.readyWG.Done()
	ticker := time.NewTicker(a.opts.ProbeInterval)
	defer ticker.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		err := a.ready.CheckReady(ctx, id, probe)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			failures++
			if failures == a.opts.ProbeFailures {
				a.setProbeVerdict(id, svcName, gen, Degraded, fmt.Sprintf("probe failed %d times: %v", failures, err))
			}
			continue
		}
		if failures >= a.opts.ProbeFailures {
			a.setProbeVerdict(id, svcName, gen, Ready, "probe passing again")
		}
		failures = 0
	}
}

func (a *Agent) setProbeVerdict(id, svcName string, gen int, to State, detail string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	inst := a.instances[id]
	if inst == nil || inst.gen != gen || inst.pid == 0 {
		return
	}
	if (to == Degraded && inst.state != Ready) || (to == Ready && inst.state != Degraded) {
		return
	}
	a.transition(inst, to, detail)
	a.syncProxyLocked(svcName)
	a.gatewayRouteLocked(svcName)
	a.syncACLLocked()
	a.wake()
}

// recycleLocked tears down the current (failed-readiness) run and schedules a
// backed-off restart. Caller holds a.mu.
func (a *Agent) recycleLocked(inst *instance) {
	if inst.readyCancel != nil {
		inst.readyCancel()
		inst.readyCancel = nil
	}
	id := inst.id
	a.watcher.Unwatch(id)
	inst.pid = 0
	inst.gen++
	a.logs.stop(id)
	delay := inst.bo.next()
	inst.restartAt = time.Now().Add(delay)
	a.transition(inst, Restarting, "restart in "+delay.String())
	go a.launcher.Stop(id)
}

// syncProxyLocked recomputes the Ready backend address set for a service and
// pushes it to the proxy only when it changed. Caller holds a.mu. The proxy
// receives, per service, the addresses of exactly the instances currently in
// the Ready state — so an instance that left Ready (death/cutover) is dropped
// here BEFORE it is killed elsewhere. A no-op for non-proxied services or when
// no proxy is injected.
func (a *Agent) syncProxyLocked(svcName string) {
	if a.proxy == nil {
		return
	}
	st := a.services[svcName]
	if st == nil || !st.spec.proxied() {
		return
	}
	addrs := a.readyAddrsLocked(svcName)
	if equalStrings(addrs, st.backends) {
		return
	}
	st.backends = addrs
	svc := svcName
	// Call the proxy outside the lock would risk reorder vs. kills; the
	// proxyController contract is cheap (write file / record), so we call it
	// under the lock to preserve "removed-before-killed" ordering. The fake
	// and the file-writer are both non-blocking enough for this.
	if err := a.proxy.SetBackends(svc, addrs); err != nil {
		a.logger.Printf("proxy: SetBackends(%q) failed: %v", svc, err)
	}
}

// syncACLLocked pushes the WHOLE desired depends_on graph to the space's
// east-west forward-chain ACL, so a service's Ready/dead transition — which
// may change what a declared edge resolves to — keeps the live ruleset
// current. Unlike syncProxyLocked (one service's backend set), the ACL is
// pushed as a complete graph every time, matching gatewayRouteLocked's
// whole-set contract on the control API: a partial push could leave a stale
// rule pointing at a dead replica's now-reused IP, silently widening access.
// A no-op only when no launcher supports it (a.acl == nil, e.g. the exec test
// launcher). An empty edges set is still PUSHED (not skipped): a re-apply
// that DROPS a service's depends_on must reopen east-west for it, the same
// way a route removed from gateway.routes un-routes rather than leaving a
// stale rule behind. Caller holds a.mu.
func (a *Agent) syncACLLocked() {
	if a.acl == nil {
		return
	}
	var edges []ACLEdge
	for name, st := range a.services {
		for _, dep := range st.spec.DependsOn {
			edges = append(edges, ACLEdge{From: name, To: dep})
		}
	}
	if err := a.acl.SetACLEdges(edges); err != nil {
		a.logger.Printf("acl: SetACLEdges failed: %v", err)
	}
}

// gatewayRouteLocked declares (or refreshes) the host-gateway route for a
// DOMAINED service, so the shared :443 front door points at the service's current
// Ready backends with TLS. It is the gateway-path sibling of syncProxyLocked:
// called wherever the Ready backend set may have moved (gate-to-Ready, death).
// pushL7RoutesLocked builds the native L7 ingress table from every routed
// service and pushes it to the space worker. Each (host, path) maps to the
// service's DNS name:port backend — the worker reverse-proxies it in-netns. A
// no-op when no native L7 router is wired (exec test launcher / singleton mode).
// Caller holds a.mu. A push error is logged, never fatal.
func (a *Agent) pushL7RoutesLocked() {
	if a.l7 == nil {
		return
	}
	var routes []ingress.Route
	for svcName, rs := range a.routesBySvc {
		st := a.services[svcName]
		if st == nil {
			continue
		}
		backend := fmt.Sprintf("%s:%d", svcName, st.spec.Port)
		for _, r := range rs {
			if r.Passthrough > 0 {
				continue
			}
			routes = append(routes, ingress.Route{
				Host: r.Host, Path: r.Path, Backend: backend,
				Rate:            ingress.RateSpec{RPS: r.Rate, Burst: r.Burst},
				Timeout:         r.Timeout,
				MaxBody:         r.MaxBody,
				RedirectApex:    r.RedirectApex,
				SecurityHeaders: r.SecurityHeaders,
				Compress:        r.Compress,
			})
		}
	}
	payload, err := json.Marshal(routes)
	if err != nil {
		a.logger.Printf("l7: marshal routes: %v", err)
		return
	}
	if err := a.l7.SetL7Routes(string(payload)); err != nil {
		a.logger.Printf("l7: SetL7Routes failed: %v", err)
	}
}

// A no-op when no gateway is wired or the service has no Domain. Caller holds
// a.mu. Like the proxy SetBackends path, a Route error is logged, never fatal.
func (a *Agent) gatewayRouteLocked(svcName string) {
	// The native in-worker L7 ingress owns the WHOLE table (not per-service), so
	// refresh it on every route-affecting event — including when there is no
	// singleton gateway (per-space mode). This is what makes a service's routes
	// reach the native ingress the moment it becomes ready.
	a.pushL7RoutesLocked()

	if a.gateway == nil {
		return
	}
	routes := a.routesBySvc[svcName]
	if len(routes) == 0 {
		return
	}
	if rspace, rsvc, mesh := splitMeshTarget(svcName); mesh {
		a.gatewayMeshRouteLocked(svcName, rspace, rsvc, routes)
		return
	}
	routes = httpRoutes(routes)
	if len(routes) == 0 {
		if err := a.gateway.Unroute(a.opts.Space, svcName); err != nil {
			a.logger.Printf("gateway: Unroute(%s/%s) (passthrough only) failed: %v", a.opts.Space, svcName, err)
		}
		return
	}
	st := a.services[svcName]
	if st == nil {
		return
	}
	backends := a.gatewayBackendsLocked(svcName)
	// No backend yet (service not Ready, no resolvable address): writing a route
	// now would emit a traefik service with an empty `servers:` list, which
	// traefik rejects ("loadBalancer cannot be a standalone element") and drops
	// the WHOLE file — so the route 404s and never recovers even after the
	// service comes up. Unroute instead (idempotent) so no broken file exists;
	// gatewayRouteLocked is re-invoked when the service reaches Ready (its address
	// resolves), and THAT call writes the route with a real backend.
	if len(backends) == 0 {
		if err := a.gateway.Unroute(a.opts.Space, svcName); err != nil {
			a.logger.Printf("gateway: Unroute(%s/%s) (no backend yet) failed: %v", a.opts.Space, svcName, err)
		}
		return
	}
	port := strconv.Itoa(st.spec.Port)
	ds := make([]Domain, 0, len(routes))
	for _, r := range routes {
		ds = append(ds, Domain{
			Host:     r.Host,
			Path:     r.Path,
			Space:    a.opts.Space,
			Service:  svcName,
			Backends: backends,
			Port:     port,
			TLS:      isPublicDomain(r.Host),

			RedirectApex:    r.RedirectApex,
			SecurityHeaders: r.SecurityHeaders,
			Compress:        r.Compress,
			Rate:            r.Rate,
			Burst:           r.Burst,
			Timeout:         r.Timeout,
			MaxBody:         r.MaxBody,
		})
	}
	if err := a.gateway.Route(a.opts.Space, svcName, ds); err != nil {
		a.logger.Printf("gateway: Route(%s/%s) failed: %v", a.opts.Space, svcName, err)
	}
}

// groupRoutesBySvc indexes the resolved routes by their target service so the
// agent can write a service's whole route set as one vendor file. Pure helper.
func httpRoutes(routes []ResolvedRoute) []ResolvedRoute {
	out := make([]ResolvedRoute, 0, len(routes))
	for _, r := range routes {
		if r.Passthrough == 0 {
			out = append(out, r)
		}
	}
	return out
}

func groupRoutesBySvc(routes []ResolvedRoute) map[string][]ResolvedRoute {
	out := make(map[string][]ResolvedRoute, len(routes))
	for _, r := range routes {
		out[r.Service] = append(out[r.Service], r)
	}
	return out
}

// SetRoutes replaces the space's gateway route set and converges the live front
// door: every service whose route set changed (gained, lost, or altered routes)
// is re-routed — Unroute for a service that lost all its routes, Route for one
// that has routes now. Called on a re-apply so a manifest that adds or changes
// routes updates the running supervisor. Returns whether the set actually
// changed; an identical set is a no-op that returns false.
func (a *Agent) SetRoutes(routes []ResolvedRoute) bool {
	// A per-space-mode space has no singleton gateway but DOES have the native L7
	// router; proceed when either front door is wired.
	if a.gateway == nil && a.l7 == nil {
		return false
	}
	next := groupRoutesBySvc(routes)
	a.mu.Lock()
	defer a.mu.Unlock()
	prev := a.routesBySvc
	if sameRouteGroups(prev, next) {
		return false
	}
	a.routesBySvc = next
	if a.gateway != nil {
		// Union of services touched by the old OR new set: those are the ones whose
		// front-door state may have moved.
		affected := make(map[string]struct{}, len(prev)+len(next))
		for svc := range prev {
			affected[svc] = struct{}{}
		}
		for svc := range next {
			affected[svc] = struct{}{}
		}
		for svc := range affected {
			if len(next[svc]) == 0 {
				// Lost all routes: drop the vendor file (a no-op if it never existed).
				if err := a.gateway.Unroute(a.opts.Space, svc); err != nil {
					a.logger.Printf("gateway: Unroute(%s/%s) failed: %v", a.opts.Space, svc, err)
				}
				continue
			}
			// Has routes now: (re)write them with the service's current backends.
			a.gatewayRouteLocked(svc)
		}
	}
	// Push the whole route set to the native in-worker L7 ingress (it owns the
	// full table, not per-service files).
	a.pushL7RoutesLocked()
	return true
}

// sameRouteGroups reports whether two service->routes maps are identical (same
// services, same ordered routes each). Order within a service is significant
// because it comes from the deploy.yaml and drives the router file's content.
func sameRouteGroups(a, b map[string][]ResolvedRoute) bool {
	if len(a) != len(b) {
		return false
	}
	for svc, ra := range a {
		rb, ok := b[svc]
		if !ok || len(ra) != len(rb) {
			return false
		}
		for i := range ra {
			if ra[i] != rb[i] {
				return false
			}
		}
	}
	return true
}

// gatewayMeshRouteLocked declares the routes of a MESH target — a service
// hosted on another node, addressed "<space>/<service>". The backend is the
// remote node's Network Controller on :80 (its mesh IP, resolved live from
// the peer cache) with the Host rewritten to the internal name, so the whole
// existing controller-to-controller HTTP path carries the traffic. An
// unresolvable target is unrouted rather than left pointing at a stale IP.
// Caller holds a.mu.
func (a *Agent) gatewayMeshRouteLocked(key, rspace, rsvc string, routes []ResolvedRoute) {
	var meshIP, backend string
	if a.meshResolve != nil {
		if ip, ok := a.meshResolve(rspace, rsvc); ok {
			meshIP, backend = ip, ip+":80"
		}
	}
	if a.meshRouted == nil {
		a.meshRouted = map[string]string{}
	}
	stamp := meshRouteStamp(backend, routes)
	if a.meshRouted[key] == stamp {
		return
	}
	a.meshRouted[key] = stamp
	if backend == "" {
		if err := a.gateway.Unroute(a.opts.Space, key); err != nil {
			a.logger.Printf("gateway: Unroute(%s/%s) (mesh target unresolved) failed: %v", a.opts.Space, key, err)
		}
		return
	}
	ds := make([]Domain, 0, len(routes))
	for _, r := range routes {
		if r.Passthrough > 0 {
			ds = append(ds, Domain{
				Host:        r.Host,
				Space:       a.opts.Space,
				Service:     key,
				Backends:    []string{net.JoinHostPort(meshIP, strconv.Itoa(r.Passthrough))},
				Port:        strconv.Itoa(r.Passthrough),
				Passthrough: true,
			})
			continue
		}
		ds = append(ds, Domain{
			Host:        r.Host,
			Path:        r.Path,
			Space:       a.opts.Space,
			Service:     key,
			Backends:    []string{backend},
			Port:        "80",
			TLS:         isPublicDomain(r.Host),
			BackendHost: rsvc + "." + rspace + ".internal",

			RedirectApex:    r.RedirectApex,
			SecurityHeaders: r.SecurityHeaders,
			Compress:        r.Compress,
			Rate:            r.Rate,
			Burst:           r.Burst,
			Timeout:         r.Timeout,
			MaxBody:         r.MaxBody,
		})
	}
	if err := a.gateway.Route(a.opts.Space, key, ds); err != nil {
		a.logger.Printf("gateway: Route(%s/%s) (mesh) failed: %v", a.opts.Space, key, err)
	}
}

// meshRouteStamp identifies what a mesh target's router file would contain: its
// backend AND the routes rendered into it. Keying the skip-if-unchanged check on
// the backend alone left a manifest that added, moved or dropped a route
// unapplied whenever the hosting node's IP had not moved, so the front door kept
// serving a stale table. An unresolved target writes no file whatever the routes
// are, so it stamps on the backend alone.
func meshRouteStamp(backend string, routes []ResolvedRoute) string {
	if backend == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(backend)
	for _, r := range routes {
		fmt.Fprintf(&b, "\x00%#v", r)
	}
	return b.String()
}

// RefreshMeshRoutes re-resolves every mesh route against the peer cache and
// re-declares the ones whose hosting node moved (or just became resolvable).
// The netctrl Reconciler calls it each tick after refreshing peers; a no-move
// pass touches nothing (change-detected per target).
func (a *Agent) RefreshMeshRoutes() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.gateway == nil {
		return
	}
	for key := range a.routesBySvc {
		if _, _, mesh := splitMeshTarget(key); mesh {
			a.gatewayRouteLocked(key)
		}
	}
}

// maybeOpenIngressLocked opens the host->space listeners for a service the
// FIRST time it has a Ready replica (the lifecycle the prompt asks for: bind on
// "at least one replica ready", tear down at space teardown). Idempotent per
// service via ingressUp. Caller holds a.mu. A no-op when no ingress controller
// is wired (exec launcher) or the service declares no port mappings. The actual
// bind runs in a goroutine so a slow Listen never stalls the reconcile lock.
func (a *Agent) maybeOpenIngressLocked(svcName string) {
	if a.ingress == nil || a.ingressUp[svcName] {
		return
	}
	maps := a.mappings[svcName]
	if len(maps) == 0 {
		return
	}
	a.ingressUp[svcName] = true
	rt := toRuntimePortMappings(maps)
	ic := a.ingress
	go ic.OpenIngress(svcName, rt)
}

// OpenControllerIngress opens a host->space listener for svcName at hostPort,
// independent of the manifest's own ingress: declaration (a.mappings) — the
// Network Controller (spacemanager/netctrl) calls this to give itself a
// 127.0.0.1:<hostPort> path into a service that declared ONLY port:, with no
// ingress: mapping of its own. Idempotent: ingressController.OpenIngress
// itself no-ops on a repeat call for the same service. A no-op when no
// ingress controller is wired (exec launcher / standalone space).
func (a *Agent) OpenControllerIngress(svcName string, hostPort, containerPort int) {
	a.mu.Lock()
	ic := a.ingress
	a.mu.Unlock()
	if ic == nil {
		return
	}
	ic.OpenIngress(svcName, []space.PortMapping{{HostPort: hostPort, ContainerPort: containerPort}})
}

// readyAddrsLocked is the sorted set of proxy backend addresses for the Ready
// instances of a service. Caller holds a.mu.
func (a *Agent) readyAddrsLocked(svcName string) []string {
	var addrs []string
	for _, inst := range a.instances {
		if inst.svcName != svcName || inst.retired || inst.state != Ready || inst.pid == 0 {
			continue
		}
		if addr := a.launcher.Addr(inst.id); addr != "" {
			addrs = append(addrs, addr)
		}
	}
	sort.Strings(addrs)
	return addrs
}

// gatewayBackendsLocked resolves the host-reachable backends a gateway dials to
// reach a DOMAINED service (one whose resolved spec has a non-empty Domain). It
// is the gateway-path sibling of readyAddrsLocked: rather than the per-instance
// proxy addresses, it returns a SINGLE loopback addr ("127.0.0.1:<hostPort>")
// that fronts ALL the service's replicas via the space's round-robin ingress.
// G3 will hand this to Gateway.Route as Domain.Backends.
//
// Returns nil when the service has no Domain, no launcher gateway-backend
// support (exec test launcher), or no Port set (a domained service MUST
// declare a port — the container port the gateway targets). Caller holds
// a.mu. The launcher call (which may bind a loopback listener on first use) runs
// under the lock; it is memoised per service, so steady-state calls are cheap.
func (a *Agent) gatewayBackendsLocked(svcName string) []string {
	if a.gwback == nil {
		return nil
	}
	if len(a.routesBySvc[svcName]) == 0 {
		return nil
	}
	st := a.services[svcName]
	if st == nil {
		return nil
	}
	port := st.spec.Port
	if port == 0 {
		a.logger.Printf("service %q: routed but no port set (no gateway backend)", svcName)
		return nil
	}
	addr, err := a.gwback.GatewayBackend(svcName, port)
	if err != nil {
		a.logger.Printf("service %q: gateway backend unavailable: %v", svcName, err)
		return nil
	}
	if addr == "" {
		return nil
	}
	return []string{addr}
}

// Reconfigure updates the desired spec of an existing service. If the runnable
// definition changed (image/command/env), it begins a zero-downtime rolling
// update; if only Replicas/Port/Proxy changed, it just updates the target so
// the next reconcile scales/re-routes. Unknown services are ignored.
func (a *Agent) Reconfigure(desired Service) {
	a.mu.Lock()
	st := a.services[desired.Name]
	if st == nil {
		a.mu.Unlock()
		return
	}
	if st.rejected != nil && !serviceDiffers(*st.rejected, desired) {
		a.logger.Printf("service %q: refusing to re-apply the spec that was just rolled back (image %s)", desired.Name, desired.Image)
		a.mu.Unlock()
		return
	}
	a.refreshIngressLocked(desired)
	if desired.Oneshot {
		a.reconfigureJobLocked(st, desired)
		a.mu.Unlock()
		a.wake()
		return
	}
	if st.rolling {
		switch {
		case st.soaking:
			// The new generation is serving but unproven; the next spec waits
			// for the soak to settle rather than tearing down what serves now.
			pending := desired
			st.pending = &pending
		case len(st.newIDs) == 0 || !specChanged(st.newSpec, desired):
			st.newSpec = desired
		case st.stoppedOld:
			// The old writer is already gone: replace the in-flight generation
			// rather than relaunching the old spec only to stop it again.
			rec := st.inFlightRecord()
			for _, id := range st.newIDs {
				if inst := a.instances[id]; inst != nil {
					a.transition(inst, Failed, "superseded by a newer spec")
					inst.retired = true
					a.stopInstanceLocked(inst)
					delete(a.instances, id)
				}
			}
			rec.Result = RolloutSuperseded
			rec.Reason = "superseded by a newer spec"
			a.recordRolloutLocked(st, rec)
			st.newSpec = desired
			st.newIDs = nil
			st.soaking = false
			st.soakAt = time.Time{}
			st.newCrashes = 0
			st.rolloutGen++
			st.rolloutID = newRolloutID(desired.Name, st.rolloutGen)
			st.rolloutStart = time.Now()
		default:
			a.abortRolloutLocked(st, RolloutSuperseded, "superseded by a newer spec", 0)
			if specChanged(st.spec, desired) {
				a.beginRolloutLocked(st, desired)
			} else {
				st.spec = desired
			}
		}
		a.mu.Unlock()
		a.wake()
		return
	}
	if !specChanged(st.spec, desired) {
		st.spec = desired // scale/route-only change; reconcile handles it
		a.mu.Unlock()
		a.wake()
		return
	}
	a.beginRolloutLocked(st, desired)
	a.mu.Unlock()
	a.wake()
}

// refreshIngressLocked re-derives desired's port mappings and, when its host
// ingress is already open, hands them to the launcher so open listeners follow
// the new container port and PROXY protocol flag. Caller holds a.mu.
func (a *Agent) refreshIngressLocked(desired Service) {
	maps, err := servicePortMappings(desired)
	if err != nil {
		a.logger.Printf("service %q: ignoring port mappings: %v", desired.Name, err)
		return
	}
	if len(maps) == 0 {
		delete(a.mappings, desired.Name)
	} else {
		a.mappings[desired.Name] = maps
	}
	if a.ingress == nil || !a.ingressUp[desired.Name] || len(maps) == 0 {
		return
	}
	go a.ingress.OpenIngress(desired.Name, toRuntimePortMappings(maps))
}

// reconfigureJobLocked applies a new spec to a job: it takes the spec in place
// and queues a run, because a published migration image ships to be applied.
// A rolling update would instead wait for a cutover on Done that never comes
// once the run is held Failed, stranding the service `rolling` for good with
// every later trigger aimed at a generation reconcile no longer launches.
// Caller holds a.mu.
func (a *Agent) reconfigureJobLocked(st *serviceState, desired Service) {
	changed := specChanged(st.spec, desired)
	st.spec = desired
	if !changed {
		return
	}
	for _, inst := range a.liveInstancesLocked(desired.Name) {
		is := desired
		is.Name = inst.id
		inst.svc = is
		inst.jobFails = 0
		inst.bo = newBackoff(a.opts.BackoffBase, a.opts.BackoffMax)
		inst.restartAt = time.Time{}
		inst.runTrigger = "image"
		a.transition(inst, Pending, "new image; scheduled run")
	}
}

// DesiredServiceNames returns the names of every currently-desired service, in
// desired order. It is the live set POST /v1/apply diffs an incoming manifest
// against (add the new, reconfigure the present, remove the gone).
func (a *Agent) DesiredServiceNames() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.order...)
}

// GatewayMode returns this space's authoritative front-door mode, as resolved
// by the node at supervisor construction — never by a remote apply's payload.
// The control server uses it to overwrite an incoming spec.Meta.GatewayMode
// instead of trusting client input for a node-owned property.
func (a *Agent) GatewayMode() string {
	return a.opts.GatewayMode
}

// ServiceDiffers reports whether the desired spec of an existing service differs
// from the live one — a runnable change (image/command/env) OR a scale/route
// change (replicas/expose/proxy/ports). An unknown service reports false: apply
// treats it as an add, not a reconfigure. Mirrors what Reconfigure would act on,
// so apply only reports/mutates a present service when something actually moved.
func (a *Agent) ServiceDiffers(desired Service) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.services[desired.Name]
	if st == nil {
		return false
	}
	if st.rejected != nil && !serviceDiffers(*st.rejected, desired) {
		return false
	}
	return serviceDiffers(st.targetSpec(), desired)
}

// targetSpec is the spec the service is converging to: the in-flight target
// during a rollout, the one queued behind a soak, else the steady state — so a
// re-apply of the same manifest mid-rollout is still "unchanged".
func (st *serviceState) targetSpec() Service {
	cur := st.spec
	if st.rolling {
		cur = st.newSpec
	}
	if st.pending != nil {
		cur = *st.pending
	}
	return cur
}

// DesiredService returns the spec a known service is converging to. It is what
// an apply compares an incoming image against: the manifest on disk can lag
// behind a rollback, the agent cannot.
func (a *Agent) DesiredService(name string) (Service, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.services[name]
	if st == nil {
		return Service{}, false
	}
	return st.targetSpec(), true
}

// SetDeclaredImage records the image Git declares for a running service
// without touching what runs: bookkeeping for the next apply's merge, never a
// rollout.
func (a *Agent) SetDeclaredImage(name, image string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.services[name]
	if st == nil {
		return
	}
	st.spec.DeclaredImage = image
	if st.rolling {
		st.newSpec.DeclaredImage = image
	}
	if st.pending != nil {
		st.pending.DeclaredImage = image
	}
	if st.rejected != nil {
		st.rejected.DeclaredImage = image
	}
}

// RejectedSpec reports whether desired is the spec whose rollout was just
// rolled back, which Reconfigure refuses to replay, with the rollback's reason.
func (a *Agent) RejectedSpec(desired Service) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.services[desired.Name]
	if st == nil || st.rejected == nil || serviceDiffers(*st.rejected, desired) {
		return "", false
	}
	return a.rejectionReasonLocked(st), true
}

// rejectionReasonLocked is the rollback reason a held-back spec was refused
// for. Caller holds a.mu and has already established st.rejected is set.
func (a *Agent) rejectionReasonLocked(st *serviceState) string {
	for i := len(st.rollouts) - 1; i >= 0; i-- {
		if st.rollouts[i].Result == RolloutRolledBack {
			return st.rollouts[i].Reason
		}
	}
	return "rolled back"
}

// AddService registers a brand-new desired service and wakes the reconcile loop,
// which creates its instance records and launches them exactly like an initial
// service. Reuses the same machinery NewAgent uses at construction (serviceState
// + order + parsed port mappings). A no-op if the service already exists.
// registryAuthSetter is the optional launcher capability to update its private-
// registry pull credentials (the runc launcher has it; the exec test launcher
// does not). Type-asserted, like ingressController, so a test launcher no-ops.
type registryAuthSetter interface {
	SetRegistryAuths(auths map[string]space.RegistryAuth)
}

// SetRegistryAuths forwards updated pull credentials to the launcher so later
// Starts authenticate. Called by the apply handler with the remote manifest's
// resolved auths. No-op when the launcher has no registry support.
func (a *Agent) SetRegistryAuths(auths map[string]space.RegistryAuth) {
	if ras, ok := a.launcher.(registryAuthSetter); ok {
		ras.SetRegistryAuths(auths)
	}
}

func (a *Agent) AddService(svc Service) {
	a.mu.Lock()
	if a.services[svc.Name] != nil {
		a.mu.Unlock()
		return
	}
	a.services[svc.Name] = &serviceState{spec: svc}
	a.order = append(a.order, svc.Name)
	if maps, err := servicePortMappings(svc); err != nil {
		a.logger.Printf("service %q: ignoring port mappings: %v", svc.Name, err)
	} else if len(maps) > 0 {
		a.mappings[svc.Name] = maps
	}
	// A newly-added service resets the all-ready latch: the set is not fully up
	// until this service comes up too.
	a.allReadyFired = false
	a.logger.Printf("service %q: added (reconcile will start %d instance(s))", svc.Name, svc.replicaCount())
	a.mu.Unlock()
	a.wake()
}

// RemoveService drops a service from the desired set and tears its instances
// down cleanly: drain it from the proxy backends FIRST (so no request lands on a
// stopping instance), then stop every instance (launcher.Stop also deregisters
// the container's DNS name and removes ingress on space teardown), and finally
// forget the service. Reuses stopInstanceLocked, the same teardown a rollout/
// teardown uses. A no-op for an unknown service.
func (a *Agent) RemoveService(name string) {
	a.mu.Lock()
	st := a.services[name]
	if st == nil {
		a.mu.Unlock()
		return
	}
	// Drain routing before killing: push an empty backend set so the proxy stops
	// targeting this service's instances before any is stopped.
	if a.proxy != nil && st.spec.proxied() {
		st.backends = nil
		if err := a.proxy.SetBackends(name, nil); err != nil {
			a.logger.Printf("proxy: draining %q failed: %v", name, err)
		}
	}
	// Likewise drop the host-gateway routes for a routed service BEFORE its
	// instances stop, so the front door stops sending traffic to a stopping host.
	if a.gateway != nil && len(a.routesBySvc[name]) > 0 {
		if err := a.gateway.Unroute(a.opts.Space, name); err != nil {
			a.logger.Printf("gateway: Unroute(%s/%s) failed: %v", a.opts.Space, name, err)
		}
	}
	// Stop and forget every instance owned by this service (steady-state + any
	// in-flight rollout ids).
	for id, inst := range a.instances {
		if inst.svcName != name {
			continue
		}
		a.stopInstanceLocked(inst)
		delete(a.instances, id)
	}
	delete(a.services, name)
	a.order = removeString(a.order, name)
	delete(a.mappings, name)
	delete(a.ingressUp, name)
	a.logger.Printf("service %q: removed (instances torn down)", name)
	a.mu.Unlock()
	a.wake()
}

// removeString returns in with the first occurrence of s dropped, preserving
// order. Used to drop a removed service from the desired order.
func removeString(in []string, s string) []string {
	for i, v := range in {
		if v == s {
			return append(in[:i:i], in[i+1:]...)
		}
	}
	return in
}

// beginRolloutLocked starts a start-first rolling update: it spins up the new
// instances ALONGSIDE the old ones (under fresh ids in a separate id space) and
// keeps the old ones serving until the new ones are Ready. Caller holds a.mu.
func (a *Agent) beginRolloutLocked(st *serviceState, newSpec Service) {
	st.rolling = true
	st.newSpec = newSpec
	st.rolloutAt = time.Time{}
	st.oldIDs = a.serviceInstanceIDsLocked(st.spec.Name)
	st.newIDs = st.newIDs[:0]
	st.rolloutGen++
	st.stoppedOld = false
	st.soaking = false
	st.soakAt = time.Time{}
	st.newCrashes = 0
	st.rejected = nil
	st.rolloutID = newRolloutID(newSpec.Name, st.rolloutGen)
	st.rolloutStart = time.Now()

	if newSpec.singleWriter() {
		for _, id := range st.oldIDs {
			inst := a.instances[id]
			if inst == nil {
				continue
			}
			inst.retired = true
			st.stopping++
			a.stopInstanceThenWakeLocked(inst, st)
		}
		st.stoppedOld = true
		st.backends = nil
		a.syncProxyLocked(newSpec.Name)
		a.logger.Printf("service %q: stopping %d instance(s) before the new spec starts (a volume has one writer)", newSpec.Name, st.stopping)
	}
}

func (a *Agent) spawnRolloutInstancesLocked(st *serviceState) {
	newSpec := st.newSpec
	st.rolloutAt = time.Now().Add(a.opts.RolloutTimeout)
	want := newSpec.replicaCount()
	for i := 0; i < want; i++ {
		// New-generation ids carry the rollout generation (e.g. "web-v2-0") so
		// they never collide with oldIDs — which, from the second rollout
		// onward, ARE the previous rollout's "-v<gen>-" ids, still live and
		// Ready. A collision would silently clobber those instances' entries
		// in a.instances below, severing bookkeeping for still-serving
		// processes before the new generation is even readiness-checked.
		id := fmt.Sprintf("%s-v%d-%d", newSpec.Name, st.rolloutGen, i)
		st.newIDs = append(st.newIDs, id)
		a.instances[id] = a.newInstanceLocked(newSpec.Name, newSpec, id)
	}
	a.logger.Printf("service %q: rolling update started (%d new instance(s))", newSpec.Name, want)
}

func (a *Agent) stopInstanceThenWakeLocked(inst *instance, st *serviceState) {
	if inst.readyCancel != nil {
		inst.readyCancel()
		inst.readyCancel = nil
	}
	id := inst.id
	a.watcher.Unwatch(id)
	inst.pid = 0
	inst.gen++
	inst.stopped = true
	a.logs.stop(id)
	go func() {
		a.launcher.Stop(id)
		a.mu.Lock()
		st.stopping--
		a.mu.Unlock()
		a.wake()
	}()
}

// serviceInstanceIDsLocked lists the non-retired instance ids of a service.
func (a *Agent) serviceInstanceIDsLocked(svcName string) []string {
	var ids []string
	for id, inst := range a.instances {
		if inst.svcName == svcName && !inst.retired {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// advanceRollout checks the in-flight rollout of a service: when all new
// instances are Ready it cuts the proxy over to them, retires+stops the old
// instances, and finalises; on timeout it keeps the old instances, tears down
// the not-ready new ones, and marks the rollout Failed (rollback-on-failure).
func (a *Agent) advanceRollout(ctx context.Context, svcName string) {
	a.mu.Lock()
	st := a.services[svcName]
	if st == nil || !st.rolling {
		a.mu.Unlock()
		return
	}
	if len(st.newIDs) == 0 {
		if job := a.failedUpstreamJobLocked(st.newSpec.DependsOn); job != "" {
			a.abortRolloutLocked(st, RolloutRolledBack, fmt.Sprintf("upstream job %q failed; the new version was never started", job), 0)
			a.mu.Unlock()
			a.wake()
			return
		}
		if st.stopping == 0 && a.upstreamsSatisfiedLocked(st.newSpec.DependsOn) {
			a.spawnRolloutInstancesLocked(st)
			a.mu.Unlock()
			a.wake()
			return
		}
		a.mu.Unlock()
		return
	}
	if st.soaking {
		if time.Now().After(st.soakAt) {
			a.completeRolloutLocked(st)
			a.mu.Unlock()
			a.wake()
			return
		}
		a.mu.Unlock()
		return
	}

	// A oneshot job's terminal success state is Done, not Ready (it never
	// serves traffic) — count that as satisfying the rollout for it, or a
	// job's rollout never cuts over.
	allReady := len(st.newIDs) > 0
	for _, id := range st.newIDs {
		inst := a.instances[id]
		if inst == nil {
			allReady = false
			break
		}
		want := Ready
		if inst.svc.Oneshot {
			want = Done
		}
		if inst.state != want {
			allReady = false
			break
		}
	}

	switch {
	case allReady:
		a.cutoverLocked(st)
		a.mu.Unlock()
	case st.stopping == 0 && time.Now().After(st.rolloutAt):
		a.abortRolloutLocked(st, RolloutRolledBack,
			fmt.Sprintf("new instances never became Ready within %v", a.opts.RolloutTimeout), 0)
		a.mu.Unlock()
	default:
		a.mu.Unlock()
	}
}

// stopInstanceLocked cancels readiness, unwatches the instance, and kills its
// container. Caller holds a.mu; the actual Stop runs in a goroutine to avoid
// blocking the lock on a slow kill.
func (a *Agent) stopInstanceLocked(inst *instance) {
	if inst.readyCancel != nil {
		inst.readyCancel()
		inst.readyCancel = nil
	}
	id := inst.id
	a.watcher.Unwatch(id)
	inst.pid = 0
	inst.gen++
	// Stop runs async and the reconcile loop may relaunch before it lands, so
	// the death this teardown causes can arrive with a fresh pid already set.
	// Mark it, or an operator's bounce reads as a crash.
	inst.bounced = true
	a.logs.stop(id)
	go a.launcher.Stop(id)
}

// teardown stops every managed container.
func (a *Agent) teardown() {
	a.mu.Lock()
	ids := make([]string, 0, len(a.instances))
	for id, inst := range a.instances {
		if inst.readyCancel != nil {
			inst.readyCancel()
			inst.readyCancel = nil
		}
		a.watcher.Unwatch(id)
		inst.pid = 0
		inst.gen++
		ids = append(ids, id)
	}
	a.mu.Unlock()

	a.logs.stopAll()
	a.readyWG.Wait()

	for _, id := range ids {
		a.launcher.Stop(id)
	}
	if c, ok := a.launcher.(interface{ Close() }); ok {
		c.Close()
	}
	a.logger.Printf("teardown complete (%d instances stopped)", len(ids))
}

// wake nudges the event loop to run reconcile immediately (non-blocking send).
func (a *Agent) wake() {
	select {
	case a.wakeCh <- struct{}{}:
	default:
	}
}

// State returns the observable state of a service: the aggregate of its live
// instances. Ready only when ALL its (non-retired) instances are Ready; else
// the "least ready" of them. Pending if unknown.
func (a *Agent) State(name string) State {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.serviceStateLocked(name)
}

func (a *Agent) serviceStateLocked(name string) State {
	st := a.services[name]
	if st == nil {
		return Pending
	}
	insts := a.liveInstancesLocked(name)
	if len(insts) == 0 {
		return Pending
	}
	// A oneshot job's observable state is its single instance's lifecycle
	// (Pending/Running/Done/Failed): the service Ready aggregation below does not
	// apply — a job is never "Ready".
	if st.spec.Oneshot {
		return insts[0].state
	}
	allReady := true
	worst := Ready
	for _, inst := range insts {
		if inst.state != Ready {
			allReady = false
		}
		// Surface the most "interesting" non-ready state for reporting.
		if inst.state == Failed || inst.state == Restarting || inst.state == Degraded {
			worst = inst.state
		} else if inst.state == Starting && worst == Ready {
			worst = Starting
		} else if inst.state == Pending && worst == Ready {
			worst = Pending
		}
	}
	if allReady {
		return Ready
	}
	return worst
}

// liveInstancesLocked returns the instances of a service that are actually
// SERVING right now: its old (pre-rollout) generation while a rollout is in
// flight — the one still routed and counted for AllReady until cutover — else
// its steady-state slots. It must NOT return newIDs mid-rollout: the whole
// point of a start-first rollout is that AllReady/the proxy keep reporting the
// OLD generation until the new one is confirmed Ready, so a caller never
// observes a window where nothing is live.
func (a *Agent) liveInstancesLocked(name string) []*instance {
	st := a.services[name]
	if st == nil {
		return nil
	}
	var ids []string
	switch {
	case st.soaking:
		ids = st.newIDs
	case st.rolling:
		ids = st.oldIDs
	default:
		ids = st.slotIDs()
	}
	var out []*instance
	for _, id := range ids {
		if inst := a.instances[id]; inst != nil && !inst.retired {
			out = append(out, inst)
		}
	}
	return out
}

// AllReady reports whether every desired service has its full replica count
// Ready — the reconciliation target a deploy front-end blocks on.
func (a *Agent) AllReady() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.allReadyLocked()
}

// allReadyLocked is AllReady without taking the lock. Caller holds a.mu.
func (a *Agent) allReadyLocked() bool {
	for _, name := range a.order {
		st := a.services[name]
		if st == nil {
			return false
		}
		// A oneshot job gates on Done, not Ready: it is a satisfied precondition,
		// excluded from the "every service Ready" accounting. A job not yet Done
		// (Pending/Running/Failed) keeps AllReady false.
		if st.spec.Oneshot {
			if !a.jobDoneLocked(name) {
				return false
			}
			continue
		}
		// spec (not newSpec) is deliberate even mid-rollout: liveInstancesLocked
		// reports the OLD generation while rolling, and spec is still that
		// generation's replica count — newSpec's count belongs to the incoming
		// generation, which is not what is being measured here.
		insts := a.liveInstancesLocked(name)
		want := st.spec.replicaCount()
		if len(insts) < want {
			return false
		}
		for _, inst := range insts {
			if inst.state != Ready {
				return false
			}
		}
	}
	return true
}

// RecentLogLines returns the last buffered container log lines of a service —
// the tail the CLI shows for a service that never became Ready, so the user
// sees WHY without digging through the cache. Empty when log streaming was off.
func (a *Agent) RecentLogLines(name string) []string {
	return a.logs.recentLines(name)
}

// InstanceSnapshot is one instance's observable state, copied out under the
// lock for the control API. ServiceSnapshot groups a service's aggregate state
// with its live instances. These are plain value snapshots — no agent
// internals escape and the caller never holds a.mu.
type InstanceSnapshot struct {
	ID    string
	State State
	PID   int

	// IP/Port are the instance's real container address and declared port:,
	// resolved via the launcher's optional ipPortProvider. Zero when the
	// launcher doesn't support it (exec tests) or the instance has no
	// resolvable IP yet. This is how the Network Controller learns a
	// service's reachable endpoint without Space ever calling out to it.
	IP   string
	Port int

	// Restarts is how many times this instance has been (re)launched beyond
	// its first run. A snapshot is instantaneous, so a service that crashes
	// and comes back within a poll interval reads as plain Ready; paired with
	// StartedAt this is what tells that flap from a service that has simply
	// been up all along.
	Restarts int

	// StartedAt is when the CURRENT run launched, so a reader can age it.
	// Zero for an instance that has never run.
	StartedAt time.Time

	// RestartAt is when a dead instance is due to relaunch, zero when none is
	// scheduled. Backoff is the delay the next restart would wait, which grows
	// 1s→30s while a service keeps dying and resets after a stable run: a
	// backoff near its ceiling IS the crash loop, observed rather than inferred.
	RestartAt time.Time
	Backoff   time.Duration

	// JobFails counts a oneshot's non-zero exits, and Held marks one that
	// exhausted its retry cap: terminal, never relaunched, and it keeps
	// AllReady false forever. A held job and a job that failed once are both
	// Failed, so without this the permanent case is unreadable.
	JobFails int
	Held     bool
}

// ServiceSnapshot is a service's aggregate state plus its live instances.
type ServiceSnapshot struct {
	Name      string
	State     State
	Image     string
	Oneshot   bool
	Instances []InstanceSnapshot
	Rollouts  []RolloutRecord
	Runs      []JobRunRecord

	// RejectedReason is set when the service holds a spec whose rollout was
	// rolled back: every apply of that same spec is refused, so the service is
	// frozen on its previous definition until a different one arrives. The
	// refusal is otherwise only visible in the reply to the apply that hit it,
	// which leaves a service stuck with nothing to read afterwards.
	RejectedReason string
}

// Snapshot returns the full observable state of the supervised set in desired
// order: per service its aggregate State and its live (non-retired) instances.
// It is the source of truth the control server serves on GET /v1/state, so
// `sklp deploy ps` reports the supervisor's view from any directory.
func (a *Agent) Snapshot() []ServiceSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]ServiceSnapshot, 0, len(a.order))
	for _, name := range a.order {
		ss := ServiceSnapshot{Name: name, State: a.serviceStateLocked(name)}
		if st := a.services[name]; st != nil {
			// During a rollout the desired image is newSpec's: reporting spec's
			// would name the image being replaced, and a watcher reading it
			// would re-request the roll already in flight.
			ss.Image = st.spec.Image
			ss.Oneshot = st.spec.Oneshot
			if st.rolling {
				ss.Image = st.newSpec.Image
			}
			ss.Rollouts = a.rolloutsLocked(name)
			ss.Runs = append([]JobRunRecord(nil), st.runs...)
			if st.rejected != nil {
				ss.RejectedReason = a.rejectionReasonLocked(st)
			}
		}
		for _, inst := range a.liveInstancesLocked(name) {
			is := InstanceSnapshot{
				ID:        inst.id,
				State:     inst.state,
				PID:       inst.pid,
				Restarts:  inst.restarts(),
				StartedAt: inst.startedAt,
				RestartAt: inst.restartAt,
				Backoff:   inst.bo.cur,
				JobFails:  inst.jobFails,
				Held:      a.jobHeldFailedLocked(inst),
			}
			if a.ipPort != nil {
				is.IP, is.Port, _ = a.ipPort.IPPort(inst.id)
			}
			ss.Instances = append(ss.Instances, is)
		}
		out = append(out, ss)
	}
	return out
}

// LogSource is one labeled log file of a service instance: label is the prefix
// shown per line ("svc" or "svc-0" when multi-replica), path the captured
// service.log. The control server streams these for GET /v1/logs/<svc>.
type LogSource struct {
	Label string
	Path  string
}

// ServiceLogSources returns the labeled log file paths of a service's live
// instances, or nil when the service is unknown or the launcher exposes no log
// paths (exec test launcher). The control server uses it to dump/follow logs so
// `sklp deploy logs <svc>` reads the supervisor's files from any directory.
func (a *Agent) ServiceLogSources(name string) []LogSource {
	provider, ok := a.launcher.(logPathProvider)
	if !ok {
		return nil
	}
	a.mu.Lock()
	insts := a.liveInstancesLocked(name)
	a.mu.Unlock()
	if len(insts) == 0 {
		return nil
	}
	multi := len(insts) > 1
	out := make([]LogSource, 0, len(insts))
	for i, inst := range insts {
		label := name
		if multi {
			label = fmt.Sprintf("%s-%d", name, i)
		}
		if path := provider.LogPath(inst.id); path != "" {
			out = append(out, LogSource{Label: label, Path: path})
		}
	}
	return out
}

// HasService reports whether name is part of the supervised set.
func (a *Agent) HasService(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.services[name] != nil
}

// execProvider is the optional launcher capability the agent type-asserts to
// resolve the runc coordinates of a live instance for `sklp deploy exec`. The
// runc launcher implements it; the exec test launcher does not (so exec is
// unavailable there). Mirrors logPathProvider/ingressController.
type execProvider interface {
	ExecCoords(id string) (runcBin, runcRoot, containerID string)
}

// ExecTarget is the runc coordinate set needed to exec into a live instance: the
// runc binary, its state root, and the resolved container id. The control server
// passes it to runtime.RuncExecTTY / RuncExecStreams.
type ExecTarget struct {
	RuncBin     string
	RuncRoot    string
	ContainerID string
}

// ResolveExecTarget returns the exec coordinates of a LIVE instance of a service:
// it picks the first Ready instance (else any running one) so `sklp deploy exec`
// lands in a healthy replica. ok is false when the service is unknown, has no
// running instance, or the launcher cannot exec (no execProvider). Safe under the
// agent lock; resolves the id while live so a concurrent death is observed.
func (a *Agent) ResolveExecTarget(name string) (ExecTarget, bool) {
	provider, ok := a.launcher.(execProvider)
	if !ok {
		return ExecTarget{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	insts := a.liveInstancesLocked(name)
	if len(insts) == 0 {
		return ExecTarget{}, false
	}
	// Prefer a Ready instance; fall back to any with a live pid.
	var chosen *instance
	for _, inst := range insts {
		if inst.pid == 0 {
			continue
		}
		if inst.state == Ready {
			chosen = inst
			break
		}
		if chosen == nil {
			chosen = inst
		}
	}
	if chosen == nil {
		return ExecTarget{}, false
	}
	bin, root, cid := provider.ExecCoords(chosen.id)
	return ExecTarget{RuncBin: bin, RuncRoot: root, ContainerID: cid}, true
}

// RestartService restarts every live instance of a service WITHOUT a full
// re-apply: it stops them under the lock (reusing stopInstanceLocked, the same
// teardown a crash/rollout uses) and wakes the reconcile loop, which relaunches
// them at their canonical ids exactly like a recovered crash. Returns false when
// the service is unknown. Instances scheduled by the loop pick up the same spec,
// so this is a clean bounce, not a config change.
func (a *Agent) RestartService(name string) bool {
	a.mu.Lock()
	st := a.services[name]
	if st == nil {
		a.mu.Unlock()
		return false
	}
	st.rejected = nil
	for _, inst := range a.liveInstancesLocked(name) {
		// A terminal oneshot has no live run to bounce, and moving it off Done
		// would slip past reconcile's Done guard and re-run it. Re-running a job
		// is TriggerJobErr's job, reached over the control API's run route.
		if inst.svc.Oneshot && (inst.state == Done || a.jobHeldFailedLocked(inst)) {
			continue
		}
		// Stop the live run; clear restartAt so reconcile relaunches it asap
		// (no backoff for an operator-initiated restart).
		a.stopInstanceLocked(inst)
		inst.restartAt = time.Time{}
		a.transition(inst, Restarting, "restart requested")
	}
	a.logger.Printf("service %q: restart requested (instances bounced)", name)
	a.mu.Unlock()
	a.wake()
	return true
}

// Errors returned by TriggerJobErr, so a caller (the control API) can tell a
// missing service from a service that is not a job from a run already in flight
// instead of collapsing all three into one negative.
var (
	ErrJobUnknown    = errors.New("unknown service")
	ErrJobNotOneshot = errors.New("service is not a job")
	ErrJobInFlight   = errors.New("job already has a run in flight")
)

// TriggerJobErr re-runs a oneshot job that already reached a terminal state. A
// job is normally launched once and never again (reconcile only launches a
// oneshot that is Pending), which is exactly what an operator- or schedule-
// initiated run must undo: it clears the terminal state back to Pending and
// resets the failure counter and backoff so the next reconcile tick launches a
// fresh run. A run still in flight is never stacked on top of.
func (a *Agent) TriggerJobErr(name string) error {
	a.mu.Lock()
	if a.services[name] == nil {
		a.mu.Unlock()
		return ErrJobUnknown
	}
	insts := a.liveInstancesLocked(name)
	if len(insts) == 0 {
		a.mu.Unlock()
		return ErrJobUnknown
	}
	for _, inst := range insts {
		if !inst.svc.Oneshot {
			a.mu.Unlock()
			return ErrJobNotOneshot
		}
		if inst.pid != 0 || inst.launching {
			a.mu.Unlock()
			return ErrJobInFlight
		}
	}
	for _, inst := range insts {
		inst.jobFails = 0
		inst.bo = newBackoff(a.opts.BackoffBase, a.opts.BackoffMax)
		inst.restartAt = time.Time{}
		inst.runTrigger = "triggered"
		a.transition(inst, Pending, "scheduled run")
	}
	a.logger.Printf("job %q: scheduled run triggered", name)
	a.mu.Unlock()
	a.wake()
	return nil
}

// TriggerJob reports whether a run was scheduled; see TriggerJobErr for why one
// was not.
func (a *Agent) TriggerJob(name string) bool {
	return a.TriggerJobErr(name) == nil
}

// ReadyBackends returns the address set currently routed to a service (test/
// introspection helper; mirrors what was last pushed to the proxy).
func (a *Agent) ReadyBackends(name string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if st := a.services[name]; st != nil {
		return append([]string(nil), st.backends...)
	}
	return nil
}

// HostBackend returns a host-reachable address for a live instance of the
// service: a loopback port the space forwards to the container's declared port.
// Container IPs live in the space netns and are not dialable from the host.
func (a *Agent) HostBackend(name string) (string, error) {
	a.mu.Lock()
	st := a.services[name]
	gwback := a.gwback
	a.mu.Unlock()
	if st == nil {
		return "", fmt.Errorf("unknown service %s", name)
	}
	if st.spec.Port == 0 {
		return "", fmt.Errorf("%s declares no port", name)
	}
	if gwback == nil {
		return "", fmt.Errorf("%s has no host-reachable backend on this runtime", name)
	}
	return gwback.GatewayBackend(name, st.spec.Port)
}

// isPublicDomain reports whether host is a public FQDN that warrants TLS via
// ACME. Delegates to ingress.IsPublicDomain — the single source of truth shared
// with the native L7's ACME domain reconciliation.
func isPublicDomain(host string) bool {
	return ingress.IsPublicDomain(host)
}
