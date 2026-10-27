package control

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// supervisor pidfile + log paths — used by the daemonless (no-systemd) detach.
//
// When there is no systemd --user (CI, container, non-login ssh), `sklp deploy`
// detaches the __supervise daemon itself (setsid + log redirection). systemd is
// not there to stop it, so `sklp deploy delete` finds the detached daemon via a
// pidfile and SIGTERMs it (the daemon already tears down on SIGTERM). Both paths
// live under the per-space config dir, beside manifest.json.

// SupervisorLogPath is where a daemonless-detached supervisor's stdout/stderr
// are redirected (the terminal is freed). Pure.
func SupervisorLogPath(space string) string {
	return filepath.Join(spaceConfigDir(space), "supervisor.log")
}

// SupervisorPidPath is the pidfile a daemonless-detached supervisor writes so
// `delete` can signal it. Pure.
func SupervisorPidPath(space string) string {
	return filepath.Join(spaceConfigDir(space), "supervisor.pid")
}

// WriteSupervisorPid records pid in the space pidfile (atomic temp+rename),
// creating the parent dir 0700. Called by __supervise at startup.
func WriteSupervisorPid(space string, pid int) error {
	dir := spaceConfigDir(space)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create space config dir: %w", err)
	}
	path := SupervisorPidPath(space)
	tmp, err := os.CreateTemp(dir, "supervisor-*.pid.tmp")
	if err != nil {
		return fmt.Errorf("create temp pidfile: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(strconv.Itoa(pid)); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write temp pidfile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp pidfile: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("commit pidfile: %w", err)
	}
	return nil
}

// RemoveSupervisorPid deletes the space pidfile. Best-effort/idempotent —
// called at supervisor teardown.
func RemoveSupervisorPid(space string) {
	_ = os.Remove(SupervisorPidPath(space))
}

// ReadSupervisorPid reads the recorded pid. A missing pidfile yields
// (0, os.ErrNotExist); a garbage pidfile yields a decode error.
func ReadSupervisorPid(space string) (int, error) {
	data, err := os.ReadFile(SupervisorPidPath(space))
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("decode pidfile for space %q: %w", space, err)
	}
	return pid, nil
}

// processAlive reports whether pid names a live process (signal 0 probe). A
// stale pidfile (process gone) returns false so callers skip the SIGTERM.
// ProcessAlive reports whether pid names a live process.
func ProcessAlive(pid int) bool { return processAlive(pid) }

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// signal 0 performs error checking without delivering a signal: nil => alive,
	// ESRCH => gone, EPERM => alive but not ours (still "exists").
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return err == syscall.EPERM
}

// SignalSupervisorDown finds a daemonless-detached supervisor via its pidfile
// and sends SIGTERM so it tears its space down cleanly (the daemon handles
// SIGTERM). Best-effort and idempotent:
//   - no pidfile (systemd-supervised or never deployed) => (false, nil)
//   - stale pid (process gone) => remove the pidfile, (false, nil)
//   - live pid => SIGTERM, (true, nil)
//
// It returns signalled=true only when a live supervisor was signalled.
func SignalSupervisorDown(space string) (signalled bool, err error) {
	pid, rerr := ReadSupervisorPid(space)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return false, nil // no daemonless supervisor recorded
		}
		return false, rerr
	}
	if !processAlive(pid) {
		RemoveSupervisorPid(space) // stale: clean it up
		return false, nil
	}
	if kerr := syscall.Kill(pid, syscall.SIGTERM); kerr != nil {
		// Raced to exit between the probe and the signal: treat ESRCH as gone.
		if kerr == syscall.ESRCH {
			RemoveSupervisorPid(space)
			return false, nil
		}
		return false, fmt.Errorf("signal supervisor for space %q (pid %d): %w", space, pid, kerr)
	}
	return true, nil
}
