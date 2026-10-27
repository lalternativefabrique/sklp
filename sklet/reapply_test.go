package sklet

import (
	"context"
	"testing"

	"github.com/lalternativefabrique/sklp/space"
	"github.com/lalternativefabrique/sklp/spacemanager"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

// stubSupervisor is a no-container control.Supervisor for the re-apply decision
// test: it answers Health (via Serve) and records apply mutations.
type stubSupervisor struct {
	live         map[string]spacemanager.Service
	reconfigured []string
}

func (s *stubSupervisor) AllReady() bool                                    { return true }
func (s *stubSupervisor) Snapshot() []spacemanager.ServiceSnapshot          { return nil }
func (s *stubSupervisor) HasService(string) bool                            { return true }
func (s *stubSupervisor) HostBackend(string) (string, error)                { return "", nil }
func (s *stubSupervisor) ServiceLogSources(string) []spacemanager.LogSource { return nil }
func (s *stubSupervisor) RecentLogLines(string) []string                    { return nil }
func (s *stubSupervisor) SetRegistryAuths(map[string]space.RegistryAuth)    {}
func (s *stubSupervisor) AddService(svc spacemanager.Service)               { s.live[svc.Name] = svc }
func (s *stubSupervisor) RemoveService(name string)                         { delete(s.live, name) }
func (s *stubSupervisor) DesiredService(string) (spacemanager.Service, bool) {
	return spacemanager.Service{}, false
}
func (s *stubSupervisor) RejectedSpec(spacemanager.Service) (string, bool) { return "", false }
func (s *stubSupervisor) SetDeclaredImage(string, string)                  {}
func (s *stubSupervisor) ServiceDiffers(svc spacemanager.Service) bool {
	cur, ok := s.live[svc.Name]
	return ok && cur.Image != svc.Image
}
func (s *stubSupervisor) SetRoutes([]spacemanager.ResolvedRoute) bool { return false }
func (s *stubSupervisor) Reconfigure(svc spacemanager.Service) {
	s.live[svc.Name] = svc
	s.reconfigured = append(s.reconfigured, svc.Name)
}
func (s *stubSupervisor) DesiredServiceNames() []string {
	names := make([]string, 0, len(s.live))
	for n := range s.live {
		names = append(names, n)
	}
	return names
}
func (s *stubSupervisor) ResolveExecTarget(string) (spacemanager.ExecTarget, bool) {
	return spacemanager.ExecTarget{}, false
}
func (s *stubSupervisor) RestartService(string) bool             { return true }
func (s *stubSupervisor) TriggerJobErr(string) error             { return nil }
func (s *stubSupervisor) GatewayMode() string                    { return "" }
func (s *stubSupervisor) OpenControllerIngress(string, int, int) {}

// TestTryReapplyNoSupervisor: with no supervisor listening, TryReapply returns
// applied=false (the caller then starts one) and no error.
func TestTryReapplyNoSupervisor(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir()) // empty: no socket

	svcs := []spacemanager.Service{{Name: "api", Image: "img:1"}}
	meta := spacemanager.SpaceMeta{Name: "production"}
	applied, err := TryReapply(context.Background(), "production", svcs, meta, false)
	if err != nil {
		t.Fatalf("TryReapply: %v", err)
	}
	if applied {
		t.Fatal("applied=true with no supervisor; want false (start a supervisor)")
	}
}

// TestTryReapplyLiveSupervisor: with a supervisor answering on the socket,
// TryReapply talks to it (applied=true) and does NOT signal "start a 2nd
// supervisor". An identical manifest reconciles to unchanged.
func TestTryReapplyLiveSupervisor(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	sup := &stubSupervisor{live: map[string]spacemanager.Service{"api": {Name: "api", Image: "img:1"}}}
	ln, _, err := control.Listen("production")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go control.Serve(ctx, "production", sup, ln, cancel)

	svcs := []spacemanager.Service{{Name: "api", Image: "img:1"}}
	meta := spacemanager.SpaceMeta{Name: "production"}
	applied, err := TryReapply(context.Background(), "production", svcs, meta, false)
	if err != nil {
		t.Fatalf("TryReapply: %v", err)
	}
	if !applied {
		t.Fatal("applied=false against a live supervisor; want true (re-apply, no 2nd supervisor)")
	}
	if len(sup.reconfigured) != 0 {
		t.Fatalf("identical manifest reconfigured %v; want unchanged", sup.reconfigured)
	}
}

// serveStubSupervisor brings up a live control socket backed by a stub holding
// `api`, and persists a manifest owned by ownerStack so the cross-stack guard
// has something to read. Returns the stub for post-apply assertions.
func serveStubSupervisor(t *testing.T, ownerStack string) *stubSupervisor {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir()) // socket
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // manifest (ownership)

	sup := &stubSupervisor{live: map[string]spacemanager.Service{"api": {Name: "api", Image: "img:1"}}}
	ln, _, err := control.Listen("production")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go control.Serve(ctx, "production", sup, ln, cancel)

	owner := []spacemanager.Service{{Name: "api", Image: "img:1"}}
	if err := control.SaveManifest("production", owner, spacemanager.SpaceMeta{Name: "production", Stack: ownerStack}); err != nil {
		t.Fatalf("save owner manifest: %v", err)
	}
	return sup
}

// TestTryReapplyCrossStackRefused: a live space owned by stack "nats" refuses a
// re-apply from stack "postgres" (which would reconcile away nats' services).
// applied=false + error; the supervisor is left untouched.
func TestTryReapplyCrossStackRefused(t *testing.T) {
	sup := serveStubSupervisor(t, "nats")

	svcs := []spacemanager.Service{{Name: "pg", Image: "pg:16"}}
	meta := spacemanager.SpaceMeta{Name: "production", Stack: "postgres"}
	applied, err := TryReapply(context.Background(), "production", svcs, meta, false)
	if err == nil {
		t.Fatal("cross-stack re-apply succeeded; want a refusal error")
	}
	if applied {
		t.Fatal("applied=true on refused cross-stack apply; want false")
	}
	if _, ok := sup.live["api"]; !ok {
		t.Fatal("owner service `api` was mutated despite the refusal")
	}
}

// TestTryReapplyCrossStackForceAdopts: --force lets stack "postgres" adopt the
// space owned by "nats"; the apply goes through (applied=true) and reconciles.
func TestTryReapplyCrossStackForceAdopts(t *testing.T) {
	serveStubSupervisor(t, "nats")

	svcs := []spacemanager.Service{{Name: "pg", Image: "pg:16"}}
	meta := spacemanager.SpaceMeta{Name: "production", Stack: "postgres"}
	applied, err := TryReapply(context.Background(), "production", svcs, meta, true)
	if err != nil {
		t.Fatalf("forced adopt: %v", err)
	}
	if !applied {
		t.Fatal("applied=false with --force; want true (adopt + reconcile)")
	}
}

// TestTryReapplySameStackAllowed: re-applying the SAME owning stack is never a
// cross-stack conflict — it reconciles without --force.
func TestTryReapplySameStackAllowed(t *testing.T) {
	serveStubSupervisor(t, "nats")

	svcs := []spacemanager.Service{{Name: "api", Image: "img:1"}}
	meta := spacemanager.SpaceMeta{Name: "production", Stack: "nats"}
	applied, err := TryReapply(context.Background(), "production", svcs, meta, false)
	if err != nil {
		t.Fatalf("same-stack re-apply: %v", err)
	}
	if !applied {
		t.Fatal("applied=false re-applying the same stack; want true")
	}
}
