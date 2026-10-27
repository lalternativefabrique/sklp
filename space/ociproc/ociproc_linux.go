//go:build linux

// Package ociproc holds the low-level runc/slirp helpers shared between the
// run/space path (package runtime) and the build path (package runnerbuild).
// It is a LEAF: it imports only the stdlib, so both packages can depend on it
// without the import cycle that blocks runnerbuild from importing runtime
// directly (runtime already imports runnerbuild).
//
// The two things worth sharing here are the egress plumbing — attaching
// slirp4netns to a container netns and the IMDS/RFC1918 blackhole — because
// the blackhole is a SECURITY filter that must stay byte-identical across both
// paths; a divergent copy is exactly the silent drift we want to avoid.
package ociproc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// runTolerateECHILD runs cmd and waits for it, tolerating the exit status
// being harvested by someone else instead of us.
//
// The sklet supervisor installs a SIGCHLD subreaper whose drain loop does
// Wait4(-1): it reaps EVERY child of the process, this one included. When it
// wins the race against this cmd.Run()'s own wait, os/exec finds nothing left
// to wait on and returns "waitid: no child processes" (ECHILD) — even though
// the child ran fine and its work is already done. Left unhandled this isn't
// just a false failure: os/exec's pidfd-based Wait can then block forever
// waiting on an event that will never come, hanging the caller. See
// space/spacenet/subid.go's runMapHelper, which fixed the same race
// for newuidmap/newgidmap; this is the same fix for runc/slirp4netns, the
// other two children this supervisor launches directly with os/exec.
//
// A genuine failure still surfaces: the child writes to stderr and exits
// non-zero, which is a different error than ECHILD.
func runTolerateECHILD(cmd *exec.Cmd) error {
	err := cmd.Run()
	if err != nil && errors.Is(err, unix.ECHILD) {
		return nil
	}
	return err
}

// StartSlirp attaches slirp4netns to pid's network namespace so every process
// in that netns gets outbound connectivity (tap0 + default route). Best-effort:
// an empty slirpBin (extraction failed) or a slirp that won't start returns an
// error the caller logs and continues without Internet — a loopback-only
// workload is unaffected.
//
// It does NOT install the egress blackhole: that is done by the createContainer
// OCI hook (see blackhole_hook_linux.go / BlackholeHookSpec), which runs INSIDE
// the container netns where it holds CAP_NET_ADMIN. The old nsenter-from-host
// blackhole here silently failed on rootless netns (EPERM) — issue #610.
//
// Returns a stop func that kills slirp on teardown.
func StartSlirp(ctx context.Context, slirpBin string, pid int) (stop func(), err error) {
	if slirpBin == "" {
		return nil, fmt.Errorf("slirp4netns binary unavailable")
	}
	// --configure: tap0 + IP 10.0.2.100/24 + default route.
	// --mtu=1500: standard Ethernet MTU. A jumbo MTU (65520) breaks real
	// egress — large TCP downloads (apt/skt tool installs) leave the netns
	// in 64K frames the physical link (MTU 1500) cannot carry and, with no
	// working path-MTU discovery across slirp, are dropped, hanging the
	// download forever. Pipeline steps download from the Internet, so 1500
	// is required for correctness over loopback throughput.
	// --disable-host-loopback: containers cannot reach the host loopback.
	cmd := exec.CommandContext(ctx, slirpBin,
		"--configure", "--mtu=1500", "--disable-host-loopback",
		strconv.Itoa(pid), "tap0")
	// slirp4netns prints startup chatter ("sent/received tapfd=N for tap0") on
	// both streams. Inheriting either one writes to the terminal a live progress
	// block owns, desyncing its cursor rewind into an endlessly restacking
	// spinner, so buffer it and surface it only when slirp fails to start.
	var slirpLog SlirpLog
	cmd.Stdout = &slirpLog
	cmd.Stderr = &slirpLog
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// Give slirp a moment to wire tap0 + routes before the first egress, and
	// treat an exit in that window as the failure it is rather than letting
	// the step run with no network and fail on DNS later.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case werr := <-done:
		return nil, fmt.Errorf("slirp4netns exited during startup (%v): %s", werr, slirpLog.String())
	case <-time.After(300 * time.Millisecond):
	}

	// NOTE: the IMDS/LAN blackhole is NOT installed here. It is the job of the
	// createContainer hook (BlackholeHookSpec), which runs inside the netns with
	// CAP_NET_ADMIN — the only context where the route add actually succeeds on
	// a rootless netns. See issue #610.

	return func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}, nil
}

// StartDetached runs `runc run -d`, which detaches the container init and
// returns immediately, writing the init PID to a pid-file we read back. This is
// what lets a caller attach slirp to the live container's netns (it needs the
// PID) before the workload needs egress. stdout/stderr go to logPath.
// BeforeStart runs between `runc create` and `runc start`, with the container's
// init pid, before any workload process exists. An error aborts the launch.
type BeforeStart func(pid int) error

func StartDetached(ctx context.Context, runcBin, runcRoot, bundle, id, logPath string, systemdCgroup bool, beforeStart BeforeStart) (int, error) {
	return startDetached(ctx, runcBin, runcRoot, bundle, id, logPath, systemdCgroup, 0, "", beforeStart)
}

// StartDetachedInNS is StartDetached with runc entered into the user + mount
// namespaces of holderPID via `nsenter` first. This is the writable-rootfs path:
// a per-service overlay holder owns a mount-ns where the service's overlay is
// mounted; running runc inside that mount-ns makes the overlay merged the
// container root (writable) while the digest lower stays shared + pristine. The
// userns is the SAME shared space userns (the holder inherited it), so the veth
// move still works and the spec carries no uidMappings. The pid-file still holds
// the host-visible container PID (no pidns is entered), read back as usual.
func StartDetachedInNS(ctx context.Context, runcBin, runcRoot, bundle, id, logPath string, systemdCgroup bool, holderPID int, beforeStart BeforeStart) (int, error) {
	return startDetached(ctx, runcBin, runcRoot, bundle, id, logPath, systemdCgroup, holderPID, "", beforeStart)
}

// StartDetachedInNSPidDir is StartDetachedInNS with runc's pid file under
// pidDir instead of the bundle: runc writes it through a temp file in that
// directory, so the directory has to belong to the id runc runs as, which in
// a child user namespace is not the bundle's owner.
func StartDetachedInNSPidDir(ctx context.Context, runcBin, runcRoot, bundle, id, logPath string, systemdCgroup bool, holderPID int, pidDir string, beforeStart BeforeStart) (int, error) {
	return startDetached(ctx, runcBin, runcRoot, bundle, id, logPath, systemdCgroup, holderPID, pidDir, beforeStart)
}

func startDetached(ctx context.Context, runcBin, runcRoot, bundle, id, logPath string, systemdCgroup bool, holderPID int, pidDir string, beforeStart BeforeStart) (int, error) {
	if pidDir == "" {
		pidDir = bundle
	}
	pidFile := filepath.Join(pidDir, "init.pid")
	// The container must NOT receive an fd onto the host log file: an entrypoint
	// that runs as container-root and chowns its files before dropping privileges
	// (mongo's docker-entrypoint.sh → `gosu mongodb`) re-owns the file to a MAPPED
	// subuid (e.g. 100998), after which the supervisor (real uid) can neither read
	// it (broken `deploy logs` + `log:` probe) nor O_TRUNC it on restart. Instead
	// the supervisor owns the log end-to-end and drains the container's stdio
	// through a pipe — the conmon model. The write end is what the container sees;
	// it can chmod/chown the pipe fd all it likes without touching logPath.
	_ = os.Remove(logPath)
	// 0600: container stdio can echo secret env.
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	logErr, logWrite, err := os.Pipe()
	if err != nil {
		f.Close()
		return 0, err
	}
	// The pipe's inode is the supervisor's, 0600. An image that points
	// /var/log/*/error.log at /dev/stderr (nginx, apache) reopens fd 2 by path
	// at boot, which is a permission check on that inode: any uid the
	// container runs as, container root included once it is a subuid, must be
	// allowed to write it. Writing is all the mode grants; the file the pipe
	// drains into stays 0600 and out of the container's reach.
	_ = logWrite.Chmod(0o622)
	// Drain pipe → log file in a goroutine that outlives this call: with `run -d`
	// runc forks the container holding the write end and returns, so the pipe stays
	// open until the container's last process exits, at which point io.Copy sees
	// EOF and closes the log. The supervisor process is long-lived, so the
	// goroutine is reaped naturally.
	go func() {
		defer f.Close()
		defer logErr.Close()
		_, _ = io.Copy(f, logErr)
	}()

	global := []string{"--root", runcRoot}
	if systemdCgroup {
		global = append(global, "--systemd-cgroup")
	}
	runc := func(sub ...string) *exec.Cmd {
		args := append(append([]string{}, global...), sub...)
		if holderPID > 0 {
			nargs := append(EnterArgs(holderPID, true), runcBin)
			return exec.CommandContext(ctx, "nsenter", append(nargs, args...)...)
		}
		return exec.CommandContext(ctx, runcBin, args...)
	}
	verb := []string{"run", "-d"}
	if beforeStart != nil {
		verb = []string{"create"}
	}
	cmd := runc(append(verb, "--bundle", bundle, "--pid-file", pidFile, id)...)
	cmd.Stdin = nil
	cmd.Stdout = logWrite
	cmd.Stderr = logWrite
	runErr := runTolerateECHILD(cmd)
	// Close our copy of the write end now that runc has dup'd it into the container:
	// only the container holds it after this, so the drain goroutine sees EOF when
	// the container exits (not while we still keep a writer open).
	logWrite.Close()
	if runErr != nil {
		out, _ := os.ReadFile(logPath)
		return 0, fmt.Errorf("runc %s: %w\n--- container stdio ---\n%s", strings.Join(verb, " "), runErr, out)
	}
	raw, err := os.ReadFile(pidFile)
	if errors.Is(err, os.ErrPermission) && holderPID > 0 {
		// runc, as the holder's root, may have created it unreadable to us.
		raw, err = exec.Command("nsenter", append(EnterArgs(holderPID, true), "--", "cat", pidFile)...).Output()
	}
	if err != nil {
		return 0, fmt.Errorf("read pidfile: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, fmt.Errorf("parse pidfile %q: %w", string(raw), err)
	}
	if beforeStart == nil {
		return pid, nil
	}
	abort := func(cause error) (int, error) {
		_ = runc("delete", "--force", id).Run()
		return 0, cause
	}
	if err := beforeStart(pid); err != nil {
		return abort(err)
	}
	start := runc("start", id)
	var startOut bytes.Buffer
	start.Stdout = &startOut
	start.Stderr = &startOut
	if err := runTolerateECHILD(start); err != nil {
		return abort(fmt.Errorf("runc start: %w: %s", err, strings.TrimSpace(startOut.String())))
	}
	return pid, nil
}

// CreateContainer runs `runc create`: it sets up the container's namespaces and
// writes the init PID to a pid-file, but does NOT start the workload process —
// the init blocks waiting for `runc start`. This create/start split is the
// clean way to attach slirp to an ISOLATED netns: the netns exists (PID is
// live) yet the workload hasn't run, so there is no race where a `RUN apk add`
// hits the network before slirp is wired. Returns the init PID.
//
// stdout/stderr of the eventual workload go to logPath (runc wires the console
// at create time). systemdCgroup toggles --systemd-cgroup for rootless limits.
func CreateContainer(ctx context.Context, runcBin, runcRoot, bundle, id, logPath string, systemdCgroup bool) (int, error) {
	pidFile := filepath.Join(bundle, "init.pid")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	args := []string{"--root", runcRoot}
	if systemdCgroup {
		args = append(args, "--systemd-cgroup")
	}
	args = append(args, "create", "--bundle", bundle, "--pid-file", pidFile, id)
	cmd := exec.CommandContext(ctx, runcBin, args...)
	cmd.Stdin = nil
	cmd.Stdout = f
	cmd.Stderr = f
	if err := runTolerateECHILD(cmd); err != nil {
		out, _ := os.ReadFile(logPath)
		return 0, fmt.Errorf("runc create: %w\n--- container stdio ---\n%s", err, out)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, fmt.Errorf("read pidfile: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, fmt.Errorf("parse pidfile %q: %w", string(raw), err)
	}
	return pid, nil
}

// StartContainer runs `runc start`: it unblocks the init created by
// CreateContainer so the workload process execs. Pair it with WaitStopped to
// run a step to completion.
func StartContainer(ctx context.Context, runcBin, runcRoot, id string) error {
	return runTolerateECHILD(exec.CommandContext(ctx, runcBin, "--root", runcRoot, "start", id))
}

// DeleteContainer force-deletes a container's runc state (clears a stale id
// before a fresh launch). Best-effort.
func DeleteContainer(runcBin, runcRoot, id string) error {
	return runTolerateECHILD(exec.Command(runcBin, "--root", runcRoot, "delete", "--force", id))
}

// KillContainer is best-effort teardown of one container: kill then delete.
func KillContainer(runcBin, runcRoot, id string) {
	_ = runTolerateECHILD(exec.Command(runcBin, "--root", runcRoot, "kill", id, "SIGKILL"))
	_ = runTolerateECHILD(exec.Command(runcBin, "--root", runcRoot, "delete", "--force", id))
}

// WaitStopped polls `runc state <id>` until the container is no longer running
// (status "stopped"/"" ) or ctx is done. It returns the final status. Used by
// the build path: a detached step container must be waited on before slirp is
// torn down and the next batch runs.
func WaitStopped(ctx context.Context, runcBin, runcRoot, id string) string {
	for {
		select {
		case <-ctx.Done():
			return containerStatus(runcBin, runcRoot, id)
		default:
		}
		st := containerStatus(runcBin, runcRoot, id)
		if st == "stopped" || st == "" {
			return st
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// containerStatus parses the "status" field out of `runc state <id>`.
// Best-effort: a missing/unreadable state yields "".
func containerStatus(runcBin, runcRoot, id string) string {
	out, err := exec.Command(runcBin, "--root", runcRoot, "state", id).Output()
	if err != nil {
		return ""
	}
	const key = `"status"`
	i := strings.Index(string(out), key)
	if i < 0 {
		return ""
	}
	rest := string(out)[i+len(key):]
	if j := strings.Index(rest, ":"); j >= 0 {
		rest = rest[j+1:]
	} else {
		return ""
	}
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
