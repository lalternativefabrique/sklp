package sklet

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/lalternativefabrique/sklp/spacemanager"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

// Mode is the chosen supervisor launch mode for a fresh deploy.
type Mode int

const (
	// ModeDetached: hand the space to systemd --user (terminal freed).
	ModeDetached Mode = iota
	// ModeForeground: explicit --foreground (debug/CI), even if systemd is up.
	ModeForeground
	// ModeDetachedNoSystemd: no systemd --user; detach the supervisor ourselves
	// (setsid + log redirection) so the terminal is still freed.
	ModeDetachedNoSystemd
)

// ChooseMode is the pure mode decision (unit-tested): --foreground forces
// foreground; otherwise detach under systemd --user when available, else detach
// ourselves (daemonless setsid). The terminal is freed in BOTH detach modes.
func ChooseMode(foreground, systemdUp bool) Mode {
	if foreground {
		return ModeForeground
	}
	if systemdUp {
		return ModeDetached
	}
	return ModeDetachedNoSystemd
}

// DeployDetached hands the space to systemd --user: it writes/enables the
// per-space unit (which runs `sklet __supervise`), enables linger best-effort,
// waits for the supervisor's control socket to answer, applies the desired
// manifest, prints the outcome and RETURNS — the CLI exits while the supervisor
// keeps running under systemd.
func DeployDetached(cfg *spacemanager.DeployConfig, services []spacemanager.Service, meta spacemanager.SpaceMeta) error {
	selfExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve own binary path for the systemd unit: %w", err)
	}

	if err := control.EnsureUnit(meta.Name, selfExe, cfg.ResolvedEnvFiles()...); err != nil {
		return fmt.Errorf("install systemd --user unit for space %q: %w (inspect: journalctl --user -u %s)",
			meta.Name, err, control.UnitName(meta.Name))
	}

	// Linger is what makes the space survive a reboot; it may need polkit/root,
	// so a failure is a WARNING, not a deploy failure.
	if err := control.EnableLinger(); err != nil {
		fmt.Fprintf(os.Stderr, "sklet: warning: could not enable linger (%v); the space will NOT survive a reboot. Enable it with: loginctl enable-linger $USER\n", err)
	}

	// Wait for the supervisor (started by systemd) to bind its control socket.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := WaitSupervisorReady(ctx, meta.Name)
	if err != nil {
		return fmt.Errorf("space %q supervisor did not become ready: %w (inspect: journalctl --user -u %s)",
			meta.Name, err, control.UnitName(meta.Name))
	}

	// First explicit apply against the freshly-started supervisor.
	spec, err := control.MarshalApplySpec(services, meta)
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	res, err := client.Apply(context.Background(), spec)
	if err != nil {
		return fmt.Errorf("apply manifest to space %q: %w", meta.Name, err)
	}
	PrintApplyResult(meta.Name, res)
	fmt.Printf("space %q deployed — supervised by systemd --user (sklet logs <svc>, sklet delete)\n", meta.Name)
	return nil
}

// WaitSupervisorReady polls the space control socket until Health answers or
// ctx expires. It dials fresh each iteration because the socket appears only
// once the systemd-started supervisor binds it.
func WaitSupervisorReady(ctx context.Context, space string) (*control.Client, error) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		client, derr := control.Dial(space)
		if derr == nil {
			hctx, hcancel := context.WithTimeout(ctx, 2*time.Second)
			_, herr := client.Health(hctx)
			hcancel()
			if herr == nil {
				return client, nil
			}
			lastErr = herr
		} else {
			lastErr = derr
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// DeployDetachedNoSystemd detaches the supervisor WITHOUT systemd: it launches
// `sklet __supervise --space X` in a NEW SESSION (setsid) with its stdio
// redirected to a per-space log file, so the daemon outlives the shell and never
// pollutes the terminal. It then polls the control socket, applies the desired
// manifest, prints the outcome and RETURNS — the CLI exits while the detached
// supervisor keeps reconciling. The per-space flock (taken inside the daemon) is
// the single-supervisor guard; `sklet delete` stops the daemon via its pidfile.
// The manifest is already persisted by the caller (SaveManifest).
func DeployDetachedNoSystemd(cfg *spacemanager.DeployConfig, services []spacemanager.Service, meta spacemanager.SpaceMeta) error {
	selfExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve own binary path to detach the supervisor: %w", err)
	}

	logPath := control.SupervisorLogPath(meta.Name)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return fmt.Errorf("create supervisor log dir: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open supervisor log %s: %w", logPath, err)
	}
	defer logFile.Close() // the child dup's the fd; closing our copy is fine

	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", os.DevNull, err)
	}
	defer devNull.Close()

	cmd := BuildSuperviseCommand(selfExe, meta.Name, devNull, logFile)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start detached supervisor: %w", err)
	}
	// Do NOT Wait and do NOT kill at defer: Setsid put the child in its own
	// session (no controlling terminal) so the parent exiting sends it no SIGHUP.
	// It is now an independent daemon; we only talk to it over the socket.

	// Poll the control socket until the detached supervisor is ready.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, werr := WaitSupervisorReady(ctx, meta.Name)
	if werr != nil {
		// The daemon never bound its socket: it may be crash-looping or blocked on
		// the flock (a competing supervisor). Best-effort kill our orphan, then point
		// at the log. (A contending daemon would not be ours to kill anyway.)
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return fmt.Errorf("detached supervisor for space %q did not become ready: %w (inspect: %s)",
			meta.Name, werr, logPath)
	}

	spec, err := control.MarshalApplySpec(services, meta)
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	res, err := client.Apply(context.Background(), spec)
	if err != nil {
		return fmt.Errorf("apply manifest to space %q: %w", meta.Name, err)
	}
	PrintApplyResult(meta.Name, res)
	fmt.Printf("space %q deployed — supervised in background (no systemd); logs: sklet logs <svc>, delete: sklet delete\n", meta.Name)
	return nil
}

// BuildSuperviseCommand constructs the detached `__supervise` command: a new
// session (Setsid) with stdin from devNull and stdout/stderr to logOut, so the
// daemon survives the shell and writes nothing to the terminal. Pure aside from
// referencing the passed file handles — unit-tested without executing it.
//
// selfExe is the sklet binary: the supervisor is re-entered on THIS binary, which
// is why the local launch path lives in sklet and not in the sklp client.
func BuildSuperviseCommand(selfExe, space string, stdin, logOut *os.File) *exec.Cmd {
	cmd := exec.Command(selfExe, "__supervise", "--space", space)
	cmd.Stdin = stdin
	cmd.Stdout = logOut
	cmd.Stderr = logOut
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd
}
