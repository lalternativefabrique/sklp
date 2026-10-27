package devstack

import (
	"context"
	"io"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/pipeline"
	"github.com/lalternativefabrique/sklp/pipeline/runnerimage"
)

// fakeBackend records calls so the selection + mapping logic AND the embedded
// supervisor loop are testable without launching a real container — the unit
// tests never touch runc. All recording is mutex-guarded because embeddedRun
// drives StartService/Watch/WaitReady/Stop from several goroutines.
type fakeBackend struct {
	mu      sync.Mutex
	started []DevServiceSpec
	waited  []string
	hosts   []string // <svc>.<scope>.localhost names registered on the mux
	stopped []string
	closed  bool

	// readyErrFor fails a specific service's readiness probe (gating test).
	readyErrFor map[string]error
	// startErrFor fails a specific service's launch (launch-failure test).
	startErrFor map[string]error
	// exitFor, when present and closed, makes Watch(name) fire — simulating a
	// crash. A service with no entry stays alive until ctx is cancelled.
	exitFor map[string]chan struct{}
}

func (f *fakeBackend) StartService(_ context.Context, spec DevServiceSpec) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.startErrFor[spec.Name]; err != nil {
		return 0, err
	}
	f.started = append(f.started, spec)
	return 4242, nil
}
func (f *fakeBackend) WaitReady(_ context.Context, name string, _ *pipeline.Ready, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.waited = append(f.waited, name)
	return f.readyErrFor[name]
}
func (f *fakeBackend) RegisterHost(scope, name string, _ int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hosts = append(f.hosts, devHostname(scope, name))
}
func (f *fakeBackend) SetStopGrace(time.Duration) {}

func (f *fakeBackend) StopService(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, name)
}
func (f *fakeBackend) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
}
func (f *fakeBackend) LogPath(name string) string { return "/tmp/" + name + ".log" }

// Watch returns a channel that closes when the service's exit channel (if any)
// is closed, or when ctx is cancelled (teardown). nil exit chan = stays alive.
func (f *fakeBackend) Watch(ctx context.Context, name string) <-chan struct{} {
	f.mu.Lock()
	exit := f.exitFor[name]
	f.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
		case <-exit:
		}
	}()
	return done
}

// snapshots of the recorded slices, mutex-guarded for the test goroutine.
func (f *fakeBackend) startedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.started))
	for i, s := range f.started {
		out[i] = s.Name
	}
	return out
}
func (f *fakeBackend) stoppedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.stopped...)
}
func (f *fakeBackend) hostCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.hosts)
}
func (f *fakeBackend) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// TestChooseDevBackend_NonEmbeddedDeclines asserts the contract on the
// non-embedded build: the chooser declines (nil, false), and Up() turns that
// into a hard error (the space runtime is the only backend — no fallback). On
// the embedded build we only assert the gate is reported true (we do NOT build
// a real launcher here: that extracts runc and touches the cache, which a unit
// test must not).
func TestChooseDevBackend_NonEmbeddedDeclines(t *testing.T) {
	if EmbeddedAvailable() {
		// Embedded Linux build: gate is on; the chooser is exercised by the
		// integration path, not this unit test (no runc side effects here).
		return
	}
	b, ok := ChooseDevBackend("sklp-dev-test", false)
	if ok || b != nil {
		// On a non-embedded build the chooser declines; Up() then fails loudly
		// (no docker/process-host fallback — the space runtime is the only path).
		t.Fatalf("non-embedded build must decline (got ok=%v b=%v)", ok, b)
	}
}

// TestDevServiceSpec_ImageService_NoRepoBind is the security guard on the dev
// side: an image: service (postgres, nats) must NOT get a host repo bind-mount.
// Only run: services (the user's own code) bind the repo for hot-reload.
func TestDevServiceSpec_ImageService_NoRepoBind(t *testing.T) {
	ns := NamedService{Name: "db", Def: pipeline.Service{Image: "postgres:16", Command: []string{"postgres"}}}
	tc := devToolchain{CacheRoot: "/home/dev/.cache/skt", BinDirs: []string{"/home/dev/.cache/skt/go/bin"}}
	spec := devServiceSpec(ns, "/home/dev/repo", tc, []string{"PGDATA=/data"})

	if spec.Image != "postgres:16" {
		t.Errorf("image not carried: %q", spec.Image)
	}
	if spec.RepoMount != "" || spec.HostRepo != "" {
		t.Errorf("image: service must not bind the host repo, got mount=%q host=%q", spec.RepoMount, spec.HostRepo)
	}
	// An image: service carries no run:/toolchain wiring — the toolchain is
	// only for the user's own code (run: services).
	if spec.ShellCmd != "" || len(spec.ToolchainBinDirs) != 0 || spec.SktCacheMount != "" {
		t.Errorf("image: service must not carry run:/toolchain fields: %+v", spec)
	}
}

// TestDevServiceSpec_RunService_BaseImagePlusToolchain proves a run: service
// runs on the minimal base image with the repo bind-mounted at /workspace for
// hot-reload, the shell command preserved, and the host toolchain (bin dirs +
// skt cache mount) carried — NO per-project image.
func TestDevServiceSpec_RunService_BaseImagePlusToolchain(t *testing.T) {
	ns := NamedService{Name: "core", Def: pipeline.Service{Run: "go run ."}}
	tc := devToolchain{CacheRoot: "/home/dev/.cache/skt", BinDirs: []string{"/home/dev/.cache/skt/go/1.25/bin"}}
	spec := devServiceSpec(ns, "/home/dev/repo", tc, []string{"PORT=4100"})

	if spec.Image != defaultDevBaseImage {
		t.Errorf("run: service must run on the minimal base %q, got %q", defaultDevBaseImage, spec.Image)
	}
	if spec.ShellCmd != "go run ." {
		t.Errorf("shell cmd = %q, want 'go run .'", spec.ShellCmd)
	}
	if spec.RepoMount != defaultRepoMount || spec.HostRepo != "/home/dev/repo" {
		t.Errorf("run: service must bind repo at %s, got mount=%q host=%q", defaultRepoMount, spec.RepoMount, spec.HostRepo)
	}
	if spec.SktCacheMount != "/home/dev/.cache/skt" || len(spec.ToolchainBinDirs) != 1 {
		t.Errorf("run: service must carry the host toolchain, got mount=%q bins=%v", spec.SktCacheMount, spec.ToolchainBinDirs)
	}
}

// TestDevServiceSpec_RunService_PackagesNeedRunnerImage: a run: service whose
// project declares system packages must be flagged for the BUILT runner rootfs
// (RunnerRecipe set), and must NOT carry the bind-mount toolchain fields — the
// toolchain is baked into the rootfs instead.
func TestDevServiceSpec_RunService_PackagesNeedRunnerImage(t *testing.T) {
	ns := NamedService{Name: "core", Def: pipeline.Service{Run: "go run ."}}
	tc := devToolchain{
		CacheRoot: "/home/dev/.cache/skt",
		BinDirs:   []string{"/home/dev/.cache/skt/go/bin"},
		Recipe:    runnerimage.Runner{Packages: []string{"git", "ffmpeg"}},
	}
	spec := devServiceSpec(ns, "/home/dev/repo", tc, nil)

	if spec.RunnerRecipe == nil {
		t.Fatal("packages declared: run: service must be flagged for the runner rootfs")
	}
	if len(spec.RunnerRecipe.Packages) != 2 {
		t.Errorf("recipe not carried: %+v", spec.RunnerRecipe)
	}
	// Baked, not bind-mounted: the minimal-base toolchain wiring must be absent.
	if spec.SktCacheMount != "" || len(spec.ToolchainBinDirs) != 0 {
		t.Errorf("runner-rootfs service must not carry bind-mount toolchain, got mount=%q bins=%v", spec.SktCacheMount, spec.ToolchainBinDirs)
	}
	// Repo bind + shell command are still wired (hot-reload, the command to run).
	if spec.RepoMount != defaultRepoMount || spec.ShellCmd != "go run ." {
		t.Errorf("repo bind / shell cmd lost: mount=%q cmd=%q", spec.RepoMount, spec.ShellCmd)
	}
}

// TestDevBackend_FakeLifecycle exercises the devBackend contract end to end
// against the fake: start records the spec, stop/close are honoured. This is
// the readiness/logs/teardown mapping surface the supervisor will drive, proven
// without a real container.
func TestDevBackend_FakeLifecycle(t *testing.T) {
	var b devBackend = &fakeBackend{}
	if _, err := b.StartService(context.Background(), DevServiceSpec{Name: "core"}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if got := b.LogPath("core"); got != "/tmp/core.log" {
		t.Errorf("log path = %q", got)
	}
	b.StopService("core")
	b.Close()
	f := b.(*fakeBackend)
	if got := f.startedNames(); len(got) != 1 || got[0] != "core" {
		t.Errorf("start not recorded: %v", got)
	}
	if got := f.stoppedNames(); len(got) != 1 || got[0] != "core" || !f.isClosed() {
		t.Errorf("stop/close not recorded: stopped=%v closed=%v", got, f.isClosed())
	}
}

// devTestHarness wires the minimal context embeddedRun needs: a discard printer
// and a fresh signal channel. repoAbs uses a temp dir so writeState/removeState
// touch a throwaway location, never the real repo.
func devTestHarness(t *testing.T, names []string) (context.Context, context.CancelFunc, *logPrinter, chan os.Signal, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	printer := newLogPrinter(io.Discard, names, false)
	sigs := make(chan os.Signal, 1)
	return ctx, cancel, printer, sigs, t.TempDir()
}

func svc(name string, def pipeline.Service) NamedService {
	return NamedService{Name: name, Def: def, ReadyTimeout: DefaultReadyTimeout}
}

// runEmbeddedAsync runs embeddedRun in a goroutine and returns a channel
// delivering its exit code, so a test can drive signals / crashes and assert
// the loop terminates (no hang). Times out the whole run to fail loudly rather
// than deadlock the suite.
func runEmbeddedAsync(t *testing.T, ctx context.Context, cancel context.CancelFunc,
	f *fakeBackend, stack *Stack, repo string, printer *logPrinter, sigs chan os.Signal) <-chan int {
	t.Helper()
	out := make(chan int, 1)
	go func() {
		out <- embeddedRun(ctx, cancel, f, "sklp-dev-test", stack, repo, "",
			map[string]string{}, map[string]string{}, printer, sigs, 100)
	}()
	return out
}

func awaitExit(t *testing.T, out <-chan int) int {
	t.Helper()
	select {
	case code := <-out:
		return code
	case <-time.After(5 * time.Second):
		t.Fatal("embeddedRun did not return — possible hang in the supervisor loop")
		return -1
	}
}

// TestEmbeddedRun_LaunchOrderAndReadyGating asserts the embedded path drives the
// backend in depends_on (topo) order — db before core before web — and that the
// readiness gate is consulted for the probed services. Stack.Services is already
// topo-sorted by the loader, so order in == launch order.
func TestEmbeddedRun_LaunchOrderAndReadyGating(t *testing.T) {
	f := &fakeBackend{}
	stack := &Stack{Services: []NamedService{
		svc("db", pipeline.Service{Image: "postgres:16", Ready: &pipeline.Ready{TCP: "localhost:5432"}}),
		svc("core", pipeline.Service{Run: "go run .", Ready: &pipeline.Ready{HTTP: "http://localhost:4100/health"}}),
		svc("web", pipeline.Service{Run: "pnpm dev"}),
	}}
	ctx, cancel, printer, sigs, repo := devTestHarness(t, []string{"db", "core", "web"})
	defer cancel()
	out := runEmbeddedAsync(t, ctx, cancel, f, stack, repo, printer, sigs)

	// All three reach steady state, then Ctrl-C tears down. Give the loop a
	// moment to launch + gate before signalling.
	waitFor(t, func() bool { return len(f.startedNames()) == 3 })
	sigs <- os.Interrupt
	if code := awaitExit(t, out); code != 0 {
		t.Fatalf("clean Ctrl-C must exit 0, got %d", code)
	}

	if got := f.startedNames(); !equalSlice(got, []string{"db", "core", "web"}) {
		t.Fatalf("launch order = %v, want [db core web] (depends_on order)", got)
	}
	// db and core have probes; web does not — the gate is consulted only for the
	// probed services.
	f.mu.Lock()
	waited := append([]string(nil), f.waited...)
	f.mu.Unlock()
	if !contains(waited, "db") || !contains(waited, "core") {
		t.Errorf("readiness gate not consulted for probed services: waited=%v", waited)
	}
	if contains(waited, "web") {
		t.Errorf("web has no probe; it must not be gated: waited=%v", waited)
	}
}

// TestEmbeddedRun_ReadyGateFailureAbortsStack proves a service whose readiness
// probe fails aborts the launch (exit 1) and tears everything down — a
// dependent is never launched on top of a non-ready dependency.
func TestEmbeddedRun_ReadyGateFailureAbortsStack(t *testing.T) {
	f := &fakeBackend{readyErrFor: map[string]error{"db": context.DeadlineExceeded}}
	stack := &Stack{Services: []NamedService{
		svc("db", pipeline.Service{Image: "postgres:16", Ready: &pipeline.Ready{TCP: "localhost:5432"}}),
		svc("core", pipeline.Service{Run: "go run ."}),
	}}
	ctx, cancel, printer, sigs, repo := devTestHarness(t, []string{"db", "core"})
	defer cancel()
	out := runEmbeddedAsync(t, ctx, cancel, f, stack, repo, printer, sigs)

	if code := awaitExit(t, out); code != 1 {
		t.Fatalf("ready-probe failure must exit 1, got %d", code)
	}
	if got := f.startedNames(); contains(got, "core") {
		t.Errorf("core must NOT launch after db failed readiness: started=%v", got)
	}
	if !f.isClosed() {
		t.Error("backend.Close must run on a failed-readiness teardown")
	}
}

// TestEmbeddedRun_CrashIsSurfacedNotReconciled is the dev-vs-deploy contrast: a
// service that exits in steady state tears the stack down (exit 0, surfaced) and
// is NOT restarted. The crashed service is the one whose exit channel we close.
func TestEmbeddedRun_CrashIsSurfacedNotReconciled(t *testing.T) {
	crash := make(chan struct{})
	f := &fakeBackend{exitFor: map[string]chan struct{}{"core": crash}}
	stack := &Stack{Services: []NamedService{
		svc("db", pipeline.Service{Image: "postgres:16"}),
		svc("core", pipeline.Service{Run: "go run ."}),
	}}
	ctx, cancel, printer, sigs, repo := devTestHarness(t, []string{"db", "core"})
	defer cancel()
	out := runEmbeddedAsync(t, ctx, cancel, f, stack, repo, printer, sigs)

	// Let both reach steady state, then crash core.
	waitFor(t, func() bool { return len(f.startedNames()) == 2 })
	close(crash)

	if code := awaitExit(t, out); code != 0 {
		t.Fatalf("a steady-state crash tears down cleanly (exit 0), got %d", code)
	}
	// dev surfaces + tears down; it must NOT re-launch core (no reconciliation).
	if n := countStarts(f, "core"); n != 1 {
		t.Errorf("core started %d times — dev must NOT reconcile/restart, want 1", n)
	}
	if !f.isClosed() {
		t.Error("backend.Close must run on a crash teardown")
	}
}

// TestEmbeddedRun_TeardownStopsAllAndCloses proves Ctrl-C stops every launched
// service and Closes the backend (no container/space leak). Services stop in
// parallel, so only the set is asserted, not an order.
func TestEmbeddedRun_TeardownStopsAllAndCloses(t *testing.T) {
	f := &fakeBackend{}
	stack := &Stack{Services: []NamedService{
		svc("db", pipeline.Service{Image: "postgres:16"}),
		svc("core", pipeline.Service{Run: "go run ."}),
		svc("web", pipeline.Service{Run: "pnpm dev"}),
	}}
	ctx, cancel, printer, sigs, repo := devTestHarness(t, []string{"db", "core", "web"})
	defer cancel()
	out := runEmbeddedAsync(t, ctx, cancel, f, stack, repo, printer, sigs)

	waitFor(t, func() bool { return len(f.startedNames()) == 3 })
	sigs <- os.Interrupt
	if code := awaitExit(t, out); code != 0 {
		t.Fatalf("Ctrl-C exit = %d, want 0", code)
	}
	got := f.stoppedNames()
	sort.Strings(got)
	if !equalSlice(got, []string{"core", "db", "web"}) {
		t.Errorf("stopped = %v, want every launched service stopped", got)
	}
	if !f.isClosed() {
		t.Error("backend.Close must run on teardown — else the space leaks")
	}
}

// TestEmbeddedRun_RegistersHostnamesOnReady proves each Ready service is
// published under <svc>.<scope>.localhost on the host mux, with the per-worktree
// scope folded in. The scope is devScopeID(repo, stack) so two worktrees get
// distinct names.
func TestEmbeddedRun_RegistersHostnamesOnReady(t *testing.T) {
	f := &fakeBackend{}
	stack := &Stack{Services: []NamedService{
		svc("core", pipeline.Service{Run: "go run .", Ready: &pipeline.Ready{HTTP: "http://localhost:4100/health"}}),
		svc("web", pipeline.Service{Run: "pnpm dev", Ready: &pipeline.Ready{HTTP: "http://localhost:5273"}}),
	}}
	ctx, cancel, printer, sigs, repo := devTestHarness(t, []string{"core", "web"})
	defer cancel()
	out := runEmbeddedAsync(t, ctx, cancel, f, stack, repo, printer, sigs)

	// Wait until BOTH hostnames are registered — RegisterHost runs after
	// WaitReady, which is strictly later than StartService, so gating on
	// startedNames() alone races the registration and drops web's host.
	waitFor(t, func() bool { return f.hostCount() == 2 })
	sigs <- os.Interrupt
	awaitExit(t, out)

	scope := devScopeID(repo, "")
	f.mu.Lock()
	hosts := append([]string(nil), f.hosts...)
	f.mu.Unlock()
	want := []string{devHostname(scope, "core"), devHostname(scope, "web")}
	for _, h := range want {
		if !contains(hosts, h) {
			t.Errorf("hostname %q not registered on the mux: got %v", h, hosts)
		}
	}
}

// TestEmbeddedRun_OptionalServiceSkipped proves an optional service is never
// launched (parity with the legacy path's optional handling).
func TestEmbeddedRun_OptionalServiceSkipped(t *testing.T) {
	f := &fakeBackend{}
	stack := &Stack{Services: []NamedService{
		svc("core", pipeline.Service{Run: "go run ."}),
		svc("admin", pipeline.Service{Run: "go run ./admin", Optional: true}),
	}}
	ctx, cancel, printer, sigs, repo := devTestHarness(t, []string{"core", "admin"})
	defer cancel()
	out := runEmbeddedAsync(t, ctx, cancel, f, stack, repo, printer, sigs)

	waitFor(t, func() bool { return len(f.startedNames()) == 1 })
	sigs <- os.Interrupt
	awaitExit(t, out)

	if contains(f.startedNames(), "admin") {
		t.Errorf("optional service must be skipped, started=%v", f.startedNames())
	}
}

func countStarts(f *fakeBackend, name string) int {
	n := 0
	for _, s := range f.startedNames() {
		if s == name {
			n++
		}
	}
	return n
}

// waitFor polls cond up to 3s; fails the test if it never holds (avoids a flaky
// sleep while still bounding the wait).
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within 3s")
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
