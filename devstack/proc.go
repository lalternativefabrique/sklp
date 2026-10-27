package devstack

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// proc is one running service. Each service launches in its own
// process group (Setpgid) so SIGTERM can be propagated to the whole
// subtree — `go run` in particular spawns the compiled binary as a
// child, and without the pgid trick the child survives Ctrl-C.
type proc struct {
	name    string
	cmd     *exec.Cmd
	bcast   *logBroadcaster
	stopped chan struct{} // closed when Wait returns
	exitErr error

	// oneshot, when true, marks a process whose exit is expected
	// (typically a migration step) and must NOT trigger a stack-wide
	// shutdown. The flag is set by the caller AFTER the process has
	// completed successfully — see waitAny which filters on it.
	oneshot bool
}

// startProc launches the command and wires stdout+stderr through the
// broadcaster. The returned proc is already running; the caller is
// expected to call WaitReady (when the service has a Ready probe) or
// just leave it running.
func startProc(ctx context.Context, name, run, workdir string, env []string, printer *logPrinter) (*proc, error) {
	// We use sh -c so users can write `run: cd foo && bar` or pipe.
	// Setpgid puts the shell + everything it spawns into one process
	// group whose ID equals the shell's PID; we kill that group on
	// shutdown.
	cmd := exec.CommandContext(ctx, "sh", "-c", run)
	cmd.Dir = workdir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	bcast := newLogBroadcaster()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %q: %w", name, err)
	}

	p := &proc{
		name:    name,
		cmd:     cmd,
		bcast:   bcast,
		stopped: make(chan struct{}),
	}

	// One subscriber for the printer; the readiness probe (if any)
	// gets its own subscriber via bcast.Subscribe in WaitReady.
	printSub := bcast.Subscribe()
	go func() {
		for line := range printSub {
			printer.Print(name, line)
		}
	}()

	go pumpLines(stdout, bcast)
	go pumpLines(stderr, bcast)

	go func() {
		p.exitErr = cmd.Wait()
		// Give the pumps a moment to flush before closing the broadcast,
		// otherwise the very last line of a fast-exiting service can be
		// lost. The pumps return as soon as the pipes EOF, which happens
		// when Wait reaps the process — sleeping is wrong; instead, we
		// drain both pipes synchronously above. (pumpLines exits on EOF,
		// so by the time Wait returns the pipes are already closed and
		// the pump goroutines have published everything.)
		bcast.Close()
		close(p.stopped)
	}()

	return p, nil
}

// pumpLines reads `r` line by line and publishes each one. EOF or any
// read error ends the pump; the caller doesn't need the error because
// proc.Wait surfaces the underlying process exit.
func pumpLines(r io.ReadCloser, b *logBroadcaster) {
	defer r.Close()
	s := bufio.NewScanner(r)
	// Default Scanner buffer is 64KB; raise it so long compile errors
	// (Go's especially) don't get truncated mid-line.
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for s.Scan() {
		b.Publish(s.Text())
	}
}

// stop sends SIGTERM to the process group, waits up to `grace`, then
// SIGKILLs anything still alive. As a safety net we also walk the
// child tree with `pkill -P` because tools like pnpm/vite and `go run`
// frequently re-parent their workers into a fresh process group,
// escaping the Setpgid we set on the shell. Safe to call multiple
// times; grace == 0 means kill immediately.
func (p *proc) stop(grace time.Duration) {
	if p.cmd == nil || p.cmd.Process == nil {
		return
	}
	// Become a subreaper before the kill so any orphan the service forks
	// during shutdown (e.g. a `while true; sleep` re-forking ~20×/s) is
	// re-parented to us instead of init, and reapOrphans below can collect
	// it. The pgrep sweep alone races this re-fork and leaks the in-flight
	// child — the historical cause of devstack.test orphan accumulation.
	installSubreaper()

	pid := p.cmd.Process.Pid
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	killDescendants(pid, syscall.SIGTERM)

	if grace > 0 {
		select {
		case <-p.stopped:
			reapOrphans(reapBudget(grace))
			return
		case <-time.After(grace):
		}
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	killDescendants(pid, syscall.SIGKILL)
	<-p.stopped
	// Collect any orphan re-parented to us during the kill sweep. Bounded
	// so a pathological forker can never hang teardown; on non-Linux this
	// is a no-op and the pgrep sweep above is the only line of defence.
	// grace==0 (kill-now) still gets a small fixed budget — the orphan
	// re-fork window is independent of the SIGTERM grace period.
	reapOrphans(reapBudget(grace))
}

// reapBudget caps the orphan-reap wait. It tracks the SIGTERM grace period
// but never drops below a small floor, so even a kill-now teardown
// (grace==0) sweeps the brief re-fork window left by a `while`-loop service.
func reapBudget(grace time.Duration) time.Duration {
	const floor = 200 * time.Millisecond
	if grace < floor {
		return floor
	}
	return grace
}

// killDescendants walks the descendant tree rooted at pid (best effort)
// and signals every child. We shell out to pgrep because reading
// /proc/*/stat ourselves would double the LoC for no real gain on the
// platforms we support (Linux/macOS via `pgrep -P`).
func killDescendants(pid int, sig syscall.Signal) {
	out, err := exec.Command("pgrep", "-P", fmt.Sprintf("%d", pid)).Output()
	if err != nil {
		return
	}
	for _, line := range splitLines(string(out)) {
		var child int
		if _, err := fmt.Sscanf(line, "%d", &child); err != nil || child == 0 {
			continue
		}
		killDescendants(child, sig)
		_ = syscall.Kill(child, sig)
	}
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// supervisor coordinates a slice of procs sharing one printer. Its
// only job is to stop them in reverse-launch order on shutdown so
// dependent services see their dependencies still alive while they
// flush.
type supervisor struct {
	mu       sync.Mutex
	procs    []*proc
	ociStops []ociStopHook
}

// ociStopHook is registered by services launched as OCI containers.
// At shutdown the supervisor invokes each hook in reverse-launch
// order (after killing the proc tree) so `docker stop sklp-dev-<svc>`
// runs even when the container's shell wrapper is already gone.
type ociStopHook struct {
	name    string
	runtime *ociRuntime
}

func (s *supervisor) add(p *proc) {
	s.mu.Lock()
	s.procs = append(s.procs, p)
	s.mu.Unlock()
}

// snapshot builds the runState for `sklp dev down` from the procs and
// OCI hooks registered so far. supervisorPID is the `sklp dev` process
// itself. Each proc's pgid equals its PID because startProc launches
// under Setpgid with no explicit pgid.
func (s *supervisor) snapshot(supervisorPID int, network string) runState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := runState{SupervisorPID: supervisorPID, Network: network}
	for _, p := range s.procs {
		if p.cmd != nil && p.cmd.Process != nil {
			st.PGIDs = append(st.PGIDs, p.cmd.Process.Pid)
		}
	}
	for _, h := range s.ociStops {
		st.OCIContainers = append(st.OCIContainers, h.name)
		if st.OCIRuntime == "" && h.runtime != nil {
			st.OCIRuntime = h.runtime.Name
		}
	}
	return st
}

// registerOCIStop records the container name + runtime so stopAll
// can issue an explicit `<rt> stop sklp-dev-<name>` after killing
// the foreground process. Without this, a runtime that backgrounds
// the container (some podman setups) would leave it running on
// Ctrl-C.
func (s *supervisor) registerOCIStop(name string, rt *ociRuntime) {
	s.mu.Lock()
	s.ociStops = append(s.ociStops, ociStopHook{name: name, runtime: rt})
	s.mu.Unlock()
}

func (s *supervisor) stopAll(grace time.Duration) {
	s.mu.Lock()
	procs := append([]*proc(nil), s.procs...)
	hooks := append([]ociStopHook(nil), s.ociStops...)
	s.mu.Unlock()
	// Reverse order: last started, first stopped — for both the
	// process tree (children that depend on parents see them alive
	// while they flush) and the OCI containers (same reasoning).
	for i := len(procs) - 1; i >= 0; i-- {
		procs[i].stop(grace)
	}
	for i := len(hooks) - 1; i >= 0; i-- {
		runOCIStop(hooks[i])
	}
}

// runOCIStop invokes `<rt> stop <container>` with a short timeout.
// We swallow errors: at shutdown, "container not found" is success.
// Anything else (daemon down) is logged via stderr but doesn't block
// the shutdown path.
func runOCIStop(h ociStopHook) {
	argv := ociStopArgv(h.runtime, h.name)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = nil, nil
	_ = cmd.Run()
}

// waitAny blocks until any non-oneshot proc exits or ctx is canceled.
// Returns the proc that exited (nil if ctx cancelled first). Oneshot
// procs are excluded because their exit is the expected outcome
// (migration finished) — counting it as a stack failure would tear
// down the whole devstack the moment a migration completes.
func (s *supervisor) waitAny(ctx context.Context) *proc {
	s.mu.Lock()
	allProcs := append([]*proc(nil), s.procs...)
	s.mu.Unlock()

	procs := make([]*proc, 0, len(allProcs))
	for _, p := range allProcs {
		if p.oneshot {
			continue
		}
		procs = append(procs, p)
	}

	cases := make([]<-chan struct{}, 0, len(procs))
	for _, p := range procs {
		cases = append(cases, p.stopped)
	}

	// Manual select since the number of procs is dynamic.
	done := make(chan *proc, 1)
	for i, ch := range cases {
		i, ch := i, ch
		go func() {
			select {
			case <-ch:
				select {
				case done <- procs[i]:
				default:
				}
			case <-ctx.Done():
			}
		}()
	}
	select {
	case p := <-done:
		return p
	case <-ctx.Done():
		return nil
	}
}
