package sklet

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/space"
	"github.com/lalternativefabrique/sklp/spacemanager"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

// downStubSupervisor is a no-container control.Supervisor that answers the read
// routes; the server's /v1/down hook is what the down test exercises.
type downStubSupervisor struct{}

func (downStubSupervisor) AllReady() bool                                    { return true }
func (downStubSupervisor) Snapshot() []spacemanager.ServiceSnapshot          { return nil }
func (downStubSupervisor) HasService(string) bool                            { return true }
func (downStubSupervisor) HostBackend(string) (string, error)                { return "", nil }
func (downStubSupervisor) ServiceLogSources(string) []spacemanager.LogSource { return nil }
func (downStubSupervisor) RecentLogLines(string) []string                    { return nil }
func (downStubSupervisor) SetRegistryAuths(map[string]space.RegistryAuth)    {}
func (downStubSupervisor) AddService(spacemanager.Service)                   {}
func (downStubSupervisor) RemoveService(string)                              {}
func (downStubSupervisor) Reconfigure(spacemanager.Service)                  {}
func (downStubSupervisor) ServiceDiffers(spacemanager.Service) bool          { return false }
func (downStubSupervisor) DesiredService(string) (spacemanager.Service, bool) {
	return spacemanager.Service{}, false
}
func (downStubSupervisor) RejectedSpec(spacemanager.Service) (string, bool) { return "", false }
func (downStubSupervisor) SetDeclaredImage(string, string)                  {}
func (downStubSupervisor) SetRoutes([]spacemanager.ResolvedRoute) bool      { return false }
func (downStubSupervisor) DesiredServiceNames() []string                    { return nil }
func (downStubSupervisor) ResolveExecTarget(string) (spacemanager.ExecTarget, bool) {
	return spacemanager.ExecTarget{}, false
}
func (downStubSupervisor) RestartService(string) bool             { return true }
func (downStubSupervisor) TriggerJobErr(string) error             { return nil }
func (downStubSupervisor) GatewayMode() string                    { return "" }
func (downStubSupervisor) OpenControllerIngress(string, int, int) {}

// TestDownViaSocketCleanStop: against a live supervisor, DownViaSocket calls
// POST /v1/down (which triggers the supervisor shutdown), waits for the socket
// to disappear, and reports a clean stop. This is the preferred daemonless path.
func TestDownViaSocketCleanStop(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	ln, socket, err := control.Listen("production")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The shutdown hook closes the listener so the socket stops answering, exactly
	// like the real supervisor teardown (which closes the socket + removes it).
	shutdown := func() {
		cancel()
		_ = ln.Close()
	}
	go control.Serve(ctx, "production", downStubSupervisor{}, ln, shutdown)

	// Wait until the server actually answers before asking it to stop.
	waitSocketAnswers(t, socket, true)

	if !DownViaSocket("production") {
		t.Fatal("DownViaSocket=false against a live supervisor; want true (clean stop)")
	}
	waitSocketAnswers(t, socket, false)
}

// TestDownViaSocketNoSupervisor: with no socket, DownViaSocket is a clean false
// so runDown falls back to the pidfile signal.
func TestDownViaSocketNoSupervisor(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir()) // empty: no socket
	if DownViaSocket("ghost") {
		t.Fatal("DownViaSocket=true with no supervisor; want false (fall back to SIGTERM)")
	}
}

// waitSocketAnswers blocks until the socket's answering state matches want, or
// fails the test after a short deadline.
func waitSocketAnswers(t *testing.T, socket string, want bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		conn, err := net.DialTimeout("unix", socket, 100*time.Millisecond)
		answering := err == nil
		if conn != nil {
			conn.Close()
		}
		if answering == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("socket %s answering=%v, want %v (timeout)", socket, answering, want)
		case <-time.After(20 * time.Millisecond):
		}
	}
}
