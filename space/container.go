package space

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lalternativefabrique/sklp/space/ociproc"
	"golang.org/x/sys/unix"
)

// startDetached runs `runc run -d` and returns the detached init PID. It now
// delegates to the shared ociproc leaf package (reused by the build path) — a
// thin local alias keeping the run/space callers untouched.
func startDetached(ctx context.Context, runcBin, runcRoot, bundle, id, logPath string, systemdCgroup bool, beforeStart ociproc.BeforeStart) (int, error) {
	return ociproc.StartDetached(ctx, runcBin, runcRoot, bundle, id, logPath, systemdCgroup, beforeStart)
}

// startDetachedInNS is startDetached with runc entered into the user + mount
// namespaces of an overlay holder (the writable-rootfs path). holderPID is the
// space overlay holder; runc runs inside its mount-ns so the container root is
// the writable overlay merged. See ociproc.StartDetachedInNS.
func startDetachedInNS(ctx context.Context, runcBin, runcRoot, bundle, id, logPath string, systemdCgroup bool, holderPID int, beforeStart ociproc.BeforeStart) (int, error) {
	return ociproc.StartDetachedInNS(ctx, runcBin, runcRoot, bundle, id, logPath, systemdCgroup, holderPID, beforeStart)
}

func startDetachedInNSPidDir(ctx context.Context, runcBin, runcRoot, bundle, id, logPath string, systemdCgroup bool, holderPID int, pidDir string, beforeStart ociproc.BeforeStart) (int, error) {
	return ociproc.StartDetachedInNSPidDir(ctx, runcBin, runcRoot, bundle, id, logPath, systemdCgroup, holderPID, pidDir, beforeStart)
}

// runForeground runs a container synchronously to completion, returning its
// exit code. Used for Oneshot services (migrations): they join the leader's
// netns, run, and must exit before their dependents start. stdout/stderr are
// captured to logPath so a failed oneshot is diagnosable.
func runForeground(ctx context.Context, runcBin, runcRoot, bundle, id, logPath string) (int, error) {
	// 0600: see startDetached — captured stdio may contain secret env.
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	cmd := runcCommandContext(ctx, runcBin, runcRoot, "run", "--bundle", bundle, id)
	cmd.Stdin = nil
	cmd.Stdout = f
	cmd.Stderr = f
	err = cmd.Run()
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode(), nil
	}
	if err != nil {
		return 0, err
	}
	return 0, nil
}

// deleteContainer force-deletes a container's runc state without killing
// (used to clear a stale id before a fresh launch). Best-effort.
func deleteContainer(runcBin, runcRoot, id string) error {
	return runcCommand(runcBin, runcRoot, "delete", "--force", id).Run()
}

// stopGracePeriod caps how long killContainer waits for a container to exit on
// its own after SIGTERM before escalating to SIGKILL. A ceiling, not a pause:
// a container that exits promptly is deleted as soon as runc reports it.
const stopGracePeriod = 30 * time.Second

const stopPollInterval = 200 * time.Millisecond

func killContainer(runcBin, runcRoot, id string) {
	killContainerWithin(runcBin, runcRoot, id, stopGracePeriod)
}

// killContainerWithin SIGTERMs every process of the container, not only its
// init: the init of a pid namespace never receives a signal it has no handler
// for, and a `sh -c` / `go run` / `node` init has none, so a `runc kill`
// (init only) would sit out the whole grace period before the SIGKILL.
// Failures are swallowed so the rest of teardown always runs.
func killContainerWithin(runcBin, runcRoot, id string, grace time.Duration) {
	if !signalContainer(runcBin, runcRoot, id, unix.SIGTERM) {
		_ = runcCommand(runcBin, runcRoot, "delete", "--force", id).Run()
		return
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		switch containerStatus(runcBin, runcRoot, id) {
		case "stopped", "":
			_ = runcCommand(runcBin, runcRoot, "delete", "--force", id).Run()
			return
		}
		time.Sleep(stopPollInterval)
	}
	signalContainer(runcBin, runcRoot, id, unix.SIGKILL)
	_ = runcCommand(runcBin, runcRoot, "delete", "--force", id).Run()
}

// signalContainer delivers sig to every process of the container and reports
// whether anything was signalled. The process set is the container's cgroup
// when one is named after the id, else the descendants of its init; with no
// readable init it falls back to `runc kill`, which reaches init alone.
func signalContainer(runcBin, runcRoot, id string, sig unix.Signal) bool {
	pids := containerPIDs(runcBin, runcRoot, id)
	if len(pids) == 0 {
		return runcCommand(runcBin, runcRoot, "kill", id, unix.SignalName(sig)).Run() == nil
	}
	sent := false
	for _, pid := range pids {
		if killAs(runcRoot, pid, sig) == nil {
			sent = true
		}
	}
	return sent
}

func containerPIDs(runcBin, runcRoot, id string) []int {
	init := containerInitPID(runcBin, runcRoot, id)
	if init <= 0 {
		return nil
	}
	if pids := cgroupPIDs(id); len(pids) > 0 {
		return pids
	}
	return processTree(init)
}

// listContainers returns the ids of every container currently in the runc
// state root. Best-effort: a missing/empty state (no runs yet) yields an empty
// list, not an error. `runc list -q` prints one id per line.
func listContainers(runcBin, runcRoot string) []string {
	out, err := runcCommand(runcBin, runcRoot, "list", "-q").Output()
	if err != nil {
		return nil
	}
	var ids []string
	for _, line := range strings.Split(string(out), "\n") {
		if id := strings.TrimSpace(line); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// containerStatus returns the runc status of a single container ("running",
// "stopped", "created", "paused", …) by parsing `runc state <id>`. Best-effort:
// a missing/unreadable state yields "" (the caller renders it as unknown). Only
// the "status" field is needed, so we scan for it instead of decoding the whole
// JSON document (no struct/coupling to runc's state schema).
func containerStatus(runcBin, runcRoot, id string) string {
	out, err := runcCommand(runcBin, runcRoot, "state", id).Output()
	if err != nil {
		return ""
	}
	// The state JSON has a top-level `"status": "<state>"`; extract its value.
	const key = `"status"`
	i := strings.Index(string(out), key)
	if i < 0 {
		return ""
	}
	rest := string(out)[i+len(key):]
	j := strings.Index(rest, ":")
	if j < 0 {
		return ""
	}
	rest = rest[j+1:]
	a := strings.Index(rest, `"`)
	if a < 0 {
		return ""
	}
	rest = rest[a+1:]
	b := strings.Index(rest, `"`)
	if b < 0 {
		return ""
	}
	return rest[:b]
}

// reapAllContainers kills and force-deletes EVERY container in the runc state
// root, optionally filtered to ids with the given prefix. It is the teardown
// backstop against leaks: a crashed/Ctrl-C'd prior run can leave a container
// `running` in the state, and a per-id Stop only reaches the ids the current
// process tracks — this reaches the rest. Best-effort, idempotent.
func reapAllContainers(runcBin, runcRoot, prefix string) {
	for _, id := range listContainers(runcBin, runcRoot) {
		if prefix != "" && !strings.HasPrefix(id, prefix) {
			continue
		}
		killContainer(runcBin, runcRoot, id)
	}
}

// containerInitPID returns a container's init pid from `runc state <id>`, or 0
// if it cannot be read. Used to enter the container's user namespace before a
// `runc exec` (see execCommand).
func containerInitPID(runcBin, runcRoot, id string) int {
	out, err := runcCommand(runcBin, runcRoot, "state", id).Output()
	if err != nil {
		return 0
	}
	var st struct {
		Pid int `json:"pid"`
	}
	if json.Unmarshal(out, &st) != nil {
		return 0
	}
	return st.Pid
}

// execCommand builds the *exec.Cmd for a `runc exec ...` invocation, entering
// the target container's USER namespace first when it runs in a range-mapped
// userns it does not share with this process (the space/overlay path).
//
// Without this, `runc exec` from the host fails with "fchown ... operation not
// permitted": runc chowns the exec'd process's console/fds to a MAPPED uid
// (e.g. 100000+) that the host (a single-id userns) has no authority over.
// Entering the container's userns first (`nsenter -t <pid> -U`) makes us
// root-over-the-full-range, so the chown succeeds. We enter ONLY the userns, not
// the mount-ns, so the runc binary path still resolves (runc does its own mount
// setns into the container). A pid of 0 (state unreadable) or a container in our
// own userns falls back to a plain invocation.
func execCommand(ctx context.Context, runcBin, runcRoot, id string, runcArgs []string) *exec.Cmd {
	if pid := containerInitPID(runcBin, runcRoot, id); pid > 0 && containerInDistinctUserns(pid) {
		args := append([]string{"--root", runcRoot}, runcArgs...)
		nargs := append(ociproc.EnterArgs(pid, false), "--", runcBin)
		return exec.CommandContext(ctx, "nsenter", append(nargs, args...)...)
	}
	return runcCommandContext(ctx, runcBin, runcRoot, runcArgs...)
}

// containerInDistinctUserns reports whether pid runs in a user namespace
// different from ours — i.e. one we must `nsenter` to gain mapped-root authority
// over. Compares the userns inode link; on any read error it returns true (the
// nsenter path is the safe default for space services, and a stale pid simply
// makes runc fail as it would anyway).
func containerInDistinctUserns(pid int) bool {
	theirs, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/user", pid))
	if err != nil {
		return true
	}
	ours, err := os.Readlink("/proc/self/ns/user")
	if err != nil {
		return true
	}
	return theirs != ours
}

// bundlePath returns a container's bundle dir from `runc state <id>` (the state
// JSON carries a top-level "bundle"), or "" if unreadable.
func bundlePath(runcBin, runcRoot, id string) string {
	out, err := runcCommand(runcBin, runcRoot, "state", id).Output()
	if err != nil {
		return ""
	}
	var st struct {
		Bundle string `json:"bundle"`
	}
	if json.Unmarshal(out, &st) != nil {
		return ""
	}
	return st.Bundle
}

// writeProcessFile builds an OCI Process JSON (0600) for `runc exec -p`, keeping
// secret env values out of runc's argv (and thus /proc/<pid>/cmdline). runc exec
// has NO --env-file flag; -p is the only way to inject env without leaking it via
// argv. The Process is the container's own config.json process (so cwd, user,
// capabilities, terminal, rlimits all match), with args replaced by argv and env
// merged in (extra wins on key collision). The returned cleanup removes the file.
func writeProcessFile(runcBin, runcRoot, id string, env, argv []string) (path string, cleanup func(), err error) {
	proc := map[string]any{}
	if bundle := bundlePath(runcBin, runcRoot, id); bundle != "" {
		if raw, rerr := os.ReadFile(filepath.Join(bundle, "config.json")); rerr == nil {
			var cfg struct {
				Process map[string]any `json:"process"`
			}
			if json.Unmarshal(raw, &cfg) == nil && cfg.Process != nil {
				proc = cfg.Process
			}
		}
	}
	// A bare exec process needs a terminal=false default if the config did not
	// supply one; we never allocate a PTY on this non-interactive path.
	if _, ok := proc["terminal"]; !ok {
		proc["terminal"] = false
	}
	proc["args"] = argv
	proc["env"] = mergeProcEnv(proc["env"], env)

	f, err := os.CreateTemp("", "sklp-execproc-*.json")
	if err != nil {
		return "", nil, fmt.Errorf("exec process file: %w", err)
	}
	cleanup = func() { _ = os.Remove(f.Name()) }
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		cleanup()
		return "", nil, fmt.Errorf("exec process chmod: %w", err)
	}
	if err := json.NewEncoder(f).Encode(proc); err != nil {
		f.Close()
		cleanup()
		return "", nil, fmt.Errorf("exec process write: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("exec process close: %w", err)
	}
	return f.Name(), cleanup, nil
}

// mergeProcEnv overlays extra (KEY=value) onto the base process env from
// config.json, extra winning on key collision so a caller-set SKLP_RUNNER_MODE /
// secret replaces a baked default. base may be nil or a []any of strings.
func mergeProcEnv(base any, extra []string) []string {
	out := []string{}
	if arr, ok := base.([]any); ok {
		for _, v := range arr {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
	}
	for _, e := range extra {
		k, _, _ := strings.Cut(e, "=")
		replaced := false
		for i, existing := range out {
			if ek, _, _ := strings.Cut(existing, "="); ek == k {
				out[i] = e
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, e)
		}
	}
	return out
}

// runcExec runs a command inside an already-running container. Used by
// readiness probes that prefer to exercise the service from inside its
// own netns (e.g. pg_isready) rather than crossing the user-namespace
// barrier from the host.
func runcExec(ctx context.Context, runcBin, runcRoot, id string, argv ...string) error {
	cmd := execCommand(ctx, runcBin, runcRoot, id, append([]string{"exec", id}, argv...))
	return cmd.Run()
}

// RuncExecStreams runs `runc exec` inside a running container with the given
// argv, wiring stdin/stdout/stderr to the supplied streams (no PTY). It blocks
// until the command exits and returns its exit code. Used by the NON-interactive
// `sklp deploy exec <svc> -- cmd...` path (capture + exit code, docker-exec-style
// without a TTY). A nil stream is left disconnected.
func RuncExecStreams(ctx context.Context, runcBin, runcRoot, id string, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	return RuncExecStreamsEnv(ctx, runcBin, runcRoot, id, nil, argv, stdin, stdout, stderr)
}

// RuncExecStreamsEnv is RuncExecStreams with extra environment. `runc exec`
// does NOT inherit the init process environment, so any KEY=value the exec'd
// command needs (here: SKLP_RUNNER_MODE so the inner runner skips local service
// boot, and the writable cache redirects) MUST be passed explicitly. The env may
// carry opt-in secrets (e.g. SCW_SECRET_KEY), so it is handed to runc via a 0600
// Process JSON (`runc exec -p`) rather than per-var --env flags: the latter land
// in runc's argv and leak through /proc/<pid>/cmdline (world-readable) to any
// local `ps`. (runc exec has no --env-file flag; -p is the leak-free path.)
// Services are reached by name:port on the bridge — no port map is injected.
func RuncExecStreamsEnv(ctx context.Context, runcBin, runcRoot, id string, env, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	// With extra env, hand runc a full Process JSON via -p (runc exec has no
	// --env-file); without it, the plain `exec <id> <argv>` form suffices.
	var runcArgs []string
	if len(env) > 0 {
		procFile, cleanup, err := writeProcessFile(runcBin, runcRoot, id, env, argv)
		if err != nil {
			return -1, err
		}
		defer cleanup()
		runcArgs = []string{"exec", "-p", procFile, id}
	} else {
		runcArgs = append([]string{"exec", id}, argv...)
	}
	cmd := execCommand(ctx, runcBin, runcRoot, id, runcArgs)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

// RuncExecTTY runs `runc exec -t` inside a running container under a freshly
// allocated PTY, proxying the PTY master bidirectionally to peer (the hijacked
// control-socket connection). It is the server side of `sklp deploy exec <svc>`:
// the client puts the LOCAL terminal in raw mode and relays the user's keystrokes
// over peer, while the container's PTY output flows back the same way. Blocks
// until the exec'd process exits; returns its exit code.
//
// rows/cols set the initial window size (0 falls back to a sane 24x80). Live
// resize is NOT propagated for this milestone (documented limit): the window is
// sized once at start. A single raw bidi stream suffices because runc multiplexes
// the interactive session over the one PTY.
func RuncExecTTY(ctx context.Context, runcBin, runcRoot, id string, argv []string, peer io.ReadWriter, rows, cols uint16) (int, error) {
	ptmx, ptsName, err := openPTY()
	if err != nil {
		return -1, fmt.Errorf("allocate pty: %w", err)
	}
	defer ptmx.Close()

	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}
	_ = unix.IoctlSetWinsize(int(ptmx.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})

	pts, err := openPTYSlave(ptsName)
	if err != nil {
		return -1, fmt.Errorf("open pty slave: %w", err)
	}
	defer pts.Close()

	cmd := execCommand(ctx, runcBin, runcRoot, id, append([]string{"exec", "-t", id}, argv...))
	cmd.Stdin = pts
	cmd.Stdout = pts
	cmd.Stderr = pts
	// New session only — do NOT Setctty on the launcher. `runc exec -t` already
	// gives the exec'd process a controlling TTY from the pts we pass as its
	// stdio; the container side owns the ctty. Setting Setctty here makes the
	// LAUNCHER (or, when wrapped, the `nsenter` hop) try to steal a controlling
	// terminal it cannot own when the supervisor already has one (e.g. served
	// over a hijacked session) → child TIOCSCTTY EPERM, surfaced by Go as
	// "fork/exec ...: operation not permitted" before the exec. Setsid alone
	// detaches cleanly without that failure.
	cmd.SysProcAttr = &unix.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return -1, fmt.Errorf("runc exec -t: %w", err)
	}
	// The child holds the slave; close our copy so the master sees EOF when the
	// child exits (else the peer->ptmx copy would block forever on a dead exec).
	_ = pts.Close()

	// peer -> container (user keystrokes). Input is decoupled from output on
	// purpose: an empty/EOF stdin (a one-shot like `ls`) must NOT cut the output
	// path. We do not wait on this copy — for an interactive shell it blocks until
	// the peer closes; the deferred ptmx.Close lets it unwind after we return.
	go func() { _, _ = io.Copy(ptmx, peer) }()

	// container -> peer (PTY output). This is the side that MUST be fully drained
	// before we return: the caller closes the underlying conn the instant this
	// function returns, so any output still buffered in the PTY master would be
	// lost (the bug for fast one-shot commands). When the child exits it releases
	// the slave; the master then EOFs and this copy ends after relaying every byte.
	outDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(peer, ptmx)
		close(outDone)
	}()

	err = cmd.Wait()
	// Wait for the output drain to complete before returning: cmd.Wait may return
	// while bytes are still in flight from the master to the peer. The child is
	// dead (Wait returned) so the master EOFs promptly — this is a bounded wait,
	// not a hang, and the ctx still bounds the whole call.
	<-outDone
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

// openPTYSlave opens the slave side for the exec'd child's stdio. O_NOCTTY is
// what keeps the calling process alive: the agent is a session leader with no
// controlling terminal, and a plain open would make this pts its controlling
// terminal — the kernel then sends SIGHUP to the session when the exec ends
// and the terminal hangs up, which killed the node agent on every interactive
// exec. The container side acquires the tty itself through runc.
func openPTYSlave(ptsName string) (*os.File, error) {
	return os.OpenFile(ptsName, os.O_RDWR|unix.O_NOCTTY, 0)
}

// openPTY allocates a Linux PTY pair via /dev/ptmx and returns the master file
// plus the slave device path. Mirrors what creack/pty does, using only x/sys so
// no new dependency is pulled in (the runtime is Linux-only under this build tag).
func openPTY() (*os.File, string, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, "", err
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		_ = master.Close()
		return nil, "", err
	}
	// Unlock the slave so it can be opened.
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		_ = master.Close()
		return nil, "", err
	}
	return master, fmt.Sprintf("/dev/pts/%d", n), nil
}
