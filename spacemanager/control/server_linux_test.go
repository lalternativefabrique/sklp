package control

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/space"
	"github.com/lalternativefabrique/sklp/spacemanager"
)

// fakeSupervisor is a minimal in-memory Supervisor for server tests: no real
// containers, just canned snapshots and log files. The mutation methods record
// what apply asked for so apply tests can assert the diff outcome.
type fakeSupervisor struct {
	ready    bool
	services []spacemanager.ServiceSnapshot
	backends map[string]string
	sources  map[string][]spacemanager.LogSource
	recent   map[string][]string

	// Apply support: live desired set keyed by name; differs reports whether a
	// present service's spec moved. added/removed/reconfigured record mutations.
	live         map[string]spacemanager.Service
	differs      map[string]bool   // service name -> ServiceDiffers verdict
	rejects      map[string]string // service name -> rollback reason RejectedSpec reports
	declared     []string          // services SetDeclaredImage was called for
	added        []string
	removed      []string
	reconfigured []string
	registryAuth map[string]space.RegistryAuth // last SetRegistryAuths payload

	// Route support: routesSet records the last SetRoutes payload; routesChanged
	// is the bool SetRoutes returns (test controls whether it reports a change).
	routesSet     []spacemanager.ResolvedRoute
	routesChanged bool

	// Job run support: jobErrs forces the TriggerJobErr verdict per job name;
	// jobRuns records the jobs a run was scheduled for.
	jobErrs map[string]error
	jobRuns []string

	// Exec/restart support: execTargets maps a service to a resolvable target
	// (absent = no running instance -> 404); restarted records bounce requests.
	execTargets map[string]spacemanager.ExecTarget
	restarted   []string

	// gatewayMode is this fake's authoritative node-level front-door mode, the
	// value a real apply asserts against an incoming spec.Meta.GatewayMode.
	gatewayMode string
}

func (f *fakeSupervisor) AllReady() bool                           { return f.ready }
func (f *fakeSupervisor) Snapshot() []spacemanager.ServiceSnapshot { return f.services }
func (f *fakeSupervisor) HostBackend(n string) (string, error) {
	if f.backends == nil {
		return "", errors.New("no host backend")
	}
	return f.backends[n], nil
}
func (f *fakeSupervisor) RecentLogLines(n string) []string { return f.recent[n] }
func (f *fakeSupervisor) ServiceLogSources(n string) []spacemanager.LogSource {
	return f.sources[n]
}
func (f *fakeSupervisor) HasService(n string) bool {
	for _, s := range f.services {
		if s.Name == n {
			return true
		}
	}
	return false
}

func (f *fakeSupervisor) DesiredServiceNames() []string {
	names := make([]string, 0, len(f.live))
	for n := range f.live {
		names = append(names, n)
	}
	return names
}

func (f *fakeSupervisor) ServiceDiffers(svc spacemanager.Service) bool {
	return f.differs[svc.Name]
}

func (f *fakeSupervisor) DesiredService(name string) (spacemanager.Service, bool) {
	svc, ok := f.live[name]
	return svc, ok
}

func (f *fakeSupervisor) RejectedSpec(svc spacemanager.Service) (string, bool) {
	reason, ok := f.rejects[svc.Name]
	return reason, ok
}

func (f *fakeSupervisor) SetDeclaredImage(name, image string) {
	svc, ok := f.live[name]
	if !ok {
		return
	}
	svc.DeclaredImage = image
	f.live[name] = svc
	f.declared = append(f.declared, name)
}

func (f *fakeSupervisor) SetRoutes(routes []spacemanager.ResolvedRoute) bool {
	f.routesSet = routes
	return f.routesChanged
}

func (f *fakeSupervisor) SetRegistryAuths(auths map[string]space.RegistryAuth) {
	f.registryAuth = auths
}

func (f *fakeSupervisor) AddService(svc spacemanager.Service) {
	if f.live == nil {
		f.live = map[string]spacemanager.Service{}
	}
	f.live[svc.Name] = svc
	f.added = append(f.added, svc.Name)
}

func (f *fakeSupervisor) RemoveService(name string) {
	delete(f.live, name)
	f.removed = append(f.removed, name)
}

func (f *fakeSupervisor) Reconfigure(svc spacemanager.Service) {
	f.live[svc.Name] = svc
	f.reconfigured = append(f.reconfigured, svc.Name)
}

func (f *fakeSupervisor) ResolveExecTarget(name string) (spacemanager.ExecTarget, bool) {
	t, ok := f.execTargets[name]
	return t, ok
}

func (f *fakeSupervisor) GatewayMode() string { return f.gatewayMode }
func (f *fakeSupervisor) RestartService(name string) bool {
	if !f.HasService(name) {
		return false
	}
	f.restarted = append(f.restarted, name)
	return true
}

func (f *fakeSupervisor) TriggerJobErr(name string) error {
	if !f.HasService(name) {
		return spacemanager.ErrJobUnknown
	}
	if err := f.jobErrs[name]; err != nil {
		return err
	}
	f.jobRuns = append(f.jobRuns, name)
	return nil
}

func (f *fakeSupervisor) OpenControllerIngress(name string, hostPort, containerPort int) {}

// serveFake binds a UDS at the space's socket path, runs Serve against the
// fake, and returns a client plus a cancel that stops the server.
func serveFake(t *testing.T, space string, sup Supervisor) (*Client, context.CancelFunc) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	ln, _, err := Listen(space)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go Serve(ctx, space, sup, ln, cancel)
	c, err := Dial(space)
	if err != nil {
		cancel()
		t.Fatalf("dial: %v", err)
	}
	return c, cancel
}

func TestServeStateAndHealth(t *testing.T) {
	sup := &fakeSupervisor{
		ready: true,
		services: []spacemanager.ServiceSnapshot{{
			Name:  "api",
			State: spacemanager.Ready,
			Instances: []spacemanager.InstanceSnapshot{
				{ID: "api-0", State: spacemanager.Ready, PID: 100},
			},
		}},
	}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	h, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if h.Space != "production" || h.PID != os.Getpid() {
		t.Fatalf("health = %+v", h)
	}

	st, err := c.State(context.Background())
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if !st.AllReady || len(st.Services) != 1 {
		t.Fatalf("state = %+v", st)
	}
	if st.Services[0].State != "ready" || st.Services[0].Instances[0].PID != 100 {
		t.Fatalf("svc = %+v", st.Services[0])
	}
}

func TestServeLogsDump(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "service.log")
	if err := os.WriteFile(logPath, []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sup := &fakeSupervisor{
		services: []spacemanager.ServiceSnapshot{{Name: "api", State: spacemanager.Ready}},
		sources:  map[string][]spacemanager.LogSource{"api": {{Label: "api", Path: logPath}}},
	}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	var sb strings.Builder
	if err := c.Logs(context.Background(), "api", false, &sb); err != nil {
		t.Fatalf("logs: %v", err)
	}
	got := sb.String()
	if !strings.Contains(got, "[api] hello") || !strings.Contains(got, "[api] world") {
		t.Fatalf("logs dump = %q", got)
	}
}

func TestServeLogsUnknownService404(t *testing.T) {
	sup := &fakeSupervisor{services: []spacemanager.ServiceSnapshot{{Name: "api"}}}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	err := c.Logs(context.Background(), "ghost", false, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("want 404 for unknown service, got %v", err)
	}
}

func TestServeLogsFollowStopsOnClientCancel(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "service.log")
	if err := os.WriteFile(logPath, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sup := &fakeSupervisor{
		services: []spacemanager.ServiceSnapshot{{Name: "api", State: spacemanager.Ready}},
		sources:  map[string][]spacemanager.LogSource{"api": {{Label: "api", Path: logPath}}},
	}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	ctx, cancelReq := context.WithCancel(context.Background())
	var sb safeBuf
	done := make(chan error, 1)
	go func() { done <- c.Logs(ctx, "api", true, &sb) }()

	deadline := time.After(2 * time.Second)
	for sb.Len() == 0 {
		select {
		case <-deadline:
			t.Fatal("never received the first line")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	cancelReq() // simulate `logs -f` Ctrl-C

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("follow returned error after cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follow did not stop after client cancel (server follower leak)")
	}
}

// TestServeDownTriggersShutdown: POST /v1/down acks 202 then calls the shutdown
// hook. The ack must arrive BEFORE the (possibly slow) teardown, so the client
// returns promptly; the hook is invoked exactly once.
func TestServeDownTriggersShutdown(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	ln, _, err := Listen("production")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	shutdownCalled := make(chan struct{}, 1)
	shutdown := func() { shutdownCalled <- struct{}{} }

	sup := &fakeSupervisor{services: []spacemanager.ServiceSnapshot{{Name: "api"}}}
	go Serve(ctx, "production", sup, ln, shutdown)

	c, err := Dial("production")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := c.Down(context.Background()); err != nil {
		t.Fatalf("down: %v", err)
	}
	select {
	case <-shutdownCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown hook not called after POST /v1/down")
	}
}

// TestServeDownNilShutdown: a foreground supervisor (no self-stop hook) reports
// /v1/down as unsupported rather than crashing.
func TestServeDownNilShutdown(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	ln, _, err := Listen("production")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup := &fakeSupervisor{services: []spacemanager.ServiceSnapshot{{Name: "api"}}}
	go Serve(ctx, "production", sup, ln, nil)

	c, _ := Dial("production")
	err = c.Down(context.Background())
	if err == nil {
		t.Fatal("want error for /v1/down with no shutdown hook, got nil")
	}
}

// TestServeStaleSocketReplaced: Listen removes an orphan socket file that
// nobody answers, so a fresh supervisor can bind the same path.
func TestServeStaleSocketReplaced(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	socket := SocketPath("production")
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatal(err)
	}
	// Create + close a dead listener: the file lingers but nobody answers.
	ln0, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	ln0.Close() // unix listener removes the file on Close, so recreate an orphan
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	ln, _, err := Listen("production")
	if err != nil {
		t.Fatalf("Listen over stale socket: %v", err)
	}
	ln.Close()
}
