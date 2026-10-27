//go:build darwin

package plan

import (
	"fmt"
	"os/exec"
	"syscall"
)

// applyNetworkIsolation logs a warning and runs with host network.
// macOS has no equivalent to Linux's CLONE_NEWNET in user space; the
// scope of this issue is "best-effort across platforms".
func applyNetworkIsolation(_ *syscall.SysProcAttr, spec sandboxSpec) {
	if spec.Network != "none" {
		return
	}
	if spec.Stderr != nil {
		fmt.Fprintln(spec.Stderr, "[sklp] network: none requested on macOS — not enforced (host network used)")
	}
}

// isNetnsPermError is unused on macOS but kept for build symmetry.
func isNetnsPermError(_ error) bool { return false }

// applyParentDeathSignal is a no-op on macOS: there is no PR_SET_PDEATHSIG.
// (Kept for build symmetry with the Linux implementation.)
func applyParentDeathSignal(_ *syscall.SysProcAttr) {}

// applyFilesystemIsolation is a no-op on macOS: there is no Landlock.
// It mirrors applyNetworkIsolation's stub — confinement is best-effort
// across platforms and never fails a step for being unavailable.
func applyFilesystemIsolation(_ *exec.Cmd, spec sandboxSpec, _ string) {
	if spec.Filesystem == "open" {
		return
	}
	if spec.Stderr != nil {
		fmt.Fprintln(spec.Stderr, "[sklp] filesystem confinement requested on macOS — not enforced (no Landlock)")
	}
}

// MaybeRunSandboxShim is a no-op on macOS (no re-exec shim path) so
// main() can call it unconditionally for build symmetry.
func MaybeRunSandboxShim() {}
