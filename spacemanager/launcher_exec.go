package spacemanager

import (
	"context"
	"fmt"
	"os/exec"
	"sync"

	"golang.org/x/sys/unix"
)

// execLauncher runs each service as a plain detached process (svc.Command is
// the argv). It mirrors the spike's spawnDetached: Start, then Release so the
// Go runtime does not reap the child — the agent's subreaper does. This is the
// privilege-free, network-free launcher used to test the reconciliation core.
type execLauncher struct {
	mu   sync.Mutex
	pids map[string]int // service name -> last started PID, for Stop
}

func newExecLauncher() *execLauncher {
	return &execLauncher{pids: make(map[string]int)}
}

func (l *execLauncher) Start(_ context.Context, svc Service) (int, error) {
	if len(svc.Command) == 0 {
		return 0, fmt.Errorf("exec launcher: service %q has no command", svc.Name)
	}
	cmd := exec.Command(svc.Command[0], svc.Command[1:]...)
	cmd.Env = svc.Env
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("exec launcher: start %q: %w", svc.Name, err)
	}
	pid := cmd.Process.Pid
	// Detach: the subreaper inherits and reaps it, not Go's child handler.
	_ = cmd.Process.Release()
	l.mu.Lock()
	l.pids[svc.Name] = pid
	l.mu.Unlock()
	return pid, nil
}

func (l *execLauncher) Stop(id string) {
	l.mu.Lock()
	pid := l.pids[id]
	delete(l.pids, id)
	l.mu.Unlock()
	if pid > 0 {
		_ = unix.Kill(pid, unix.SIGKILL) // best-effort; ESRCH if already gone
	}
}

// Addr is a synthetic per-instance address for tests: there is no real
// network, so the proxy backend is just a stable, unique string derived from
// the instance id. It lets the proxy/rolling tests assert which instances are
// in the backend set without any container networking.
func (l *execLauncher) Addr(id string) string {
	return "exec://" + id
}
