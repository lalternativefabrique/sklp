//go:build linux

package plan

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/landlock-lsm/go-landlock/landlock"
)

// applyNetworkIsolation honours `network: none` on Linux by unsharing
// the network namespace. The freshly-cloned process starts with only
// a down loopback, so outbound traffic — including 127.0.0.1 — fails
// at the kernel level, not as a connection-refused. Falls back to
// host network when the kernel refuses (no CAP_SYS_ADMIN / userns
// nesting), with the caller logging the degradation.
func applyNetworkIsolation(attr *syscall.SysProcAttr, spec sandboxSpec) {
	if spec.Network != "none" {
		return
	}
	attr.Unshareflags |= syscall.CLONE_NEWNET
	attr.Cloneflags |= syscall.CLONE_NEWNET
}

// applyParentDeathSignal makes the kernel SIGKILL the sandboxed child when its
// parent (the sklp / test process that launched it) dies — by ANY cause, incl.
// SIGKILL or a `go test` / `pkill` of the runner. Setpgid alone cannot do this:
// a brutally-killed parent runs no handler to signal its group, and the child,
// living in its own namespaces, is otherwise reparented to PID 1 and lingers.
// Repeated interrupted runs leaked hundreds of __sandbox-exec orphans;
// PR_SET_PDEATHSIG is the only thing that fires without the parent acting. Same
// fix shape as the spacenet worker (PR #645).
func applyParentDeathSignal(attr *syscall.SysProcAttr) {
	attr.Pdeathsig = syscall.SIGKILL
}

// isNetnsPermError matches EPERM / EINVAL from a failed unshare on
// hosts without CAP_SYS_ADMIN or with userns nesting disabled.
func isNetnsPermError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EINVAL)
}

// sandboxShimEnv carries the serialised FS policy to the re-exec shim.
// Landlock self-restricts the calling process, so we must apply it in
// the child (not the parent, which materialises secrets / reads the
// cache). Go has no clean pre-exec hook for an arbitrary func on
// exec.Cmd, so we re-exec our own binary into MaybeRunSandboxShim,
// which applies Landlock then execs the real command. The shim cost is
// one extra fork+exec (<1ms), negligible per step.
const sandboxShimEnv = "SKLP_SANDBOX_FS_POLICY"

// applyFilesystemIsolation rewrites cmd so the child applies the
// confinement to itself before exec. When confinement is disabled
// (filesystem: open) or the policy is empty it is a no-op.
func applyFilesystemIsolation(cmd *exec.Cmd, spec sandboxSpec, workdir string) {
	policy := computeFSPolicy(spec, workdir)
	if policy == nil {
		return
	}
	self, err := os.Executable()
	if err != nil {
		// No re-exec target: degrade like a missing-Landlock kernel.
		if spec.Stderr != nil {
			fmt.Fprintf(spec.Stderr, "[sklp] filesystem isolation unavailable (cannot locate self: %v); running without FS confinement\n", err)
		}
		return
	}
	blob, err := json.Marshal(policy)
	if err != nil {
		if spec.Stderr != nil {
			fmt.Fprintf(spec.Stderr, "[sklp] filesystem isolation unavailable (encode policy: %v); running without FS confinement\n", err)
		}
		return
	}
	// Resolve the command to an absolute path HERE, in the parent, using
	// the parent's PATH — exactly as exec.Command already did before the
	// shim existed (cmd.Path holds Go's resolution). The shim runs with
	// the step's composed env, whose PATH may not contain the binary's
	// dir (and Landlock would block a PATH walk anyway), so it must not
	// re-run LookPath. Fall back to argv[0] if cmd.Path is unresolved.
	orig := cmd.Args
	resolved := cmd.Path
	if resolved == "" {
		resolved = orig[0]
	}
	// Re-exec self as the shim; argv after the marker is the absolute
	// program path followed by the original args.
	cmd.Path = self
	cmd.Args = append([]string{self, sandboxShimVerb, resolved}, orig[1:]...)
	cmd.Env = append(cmd.Env, sandboxShimEnv+"="+string(blob))
}

// sandboxShimVerb is the hidden first argument that routes a re-exec
// into MaybeRunSandboxShim. It is not a user-facing verb.
const sandboxShimVerb = "__sandbox-exec"

// MaybeRunSandboxShim is called at the very top of main(). When the
// process was re-exec'd as the FS-confinement shim it applies Landlock
// to itself from the serialised policy, then execs the real command
// (argv after the shim verb). It never returns on the shim path. On a
// normal invocation it returns immediately so main() proceeds.
//
// Landlock is applied with V5.BestEffort(): on kernels without Landlock
// (or a lower ABI) the call degrades to a no-op instead of failing, so a
// step is never failed merely because the host is old — mirroring the
// network namespace fallback.
func MaybeRunSandboxShim() {
	if len(os.Args) < 2 || os.Args[1] != sandboxShimVerb {
		return
	}
	cmd := os.Args[2:]
	if len(cmd) == 0 {
		fmt.Fprintln(os.Stderr, "[sklp] sandbox shim: no command to exec")
		os.Exit(127)
	}

	if blob := os.Getenv(sandboxShimEnv); blob != "" {
		var policy []fsAccess
		if err := json.Unmarshal([]byte(blob), &policy); err != nil {
			fmt.Fprintf(os.Stderr, "[sklp] sandbox shim: decode policy: %v\n", err)
			os.Exit(127)
		}
		if err := restrictLandlock(policy); err != nil {
			// V5.BestEffort() already returns nil when the kernel lacks
			// Landlock (or supports only a lower ABI) — that legitimate
			// "host too old" case degrades silently inside the call. A
			// non-nil error therefore means the RULESET was rejected (a
			// malformed grant, e.g. directory rights on a regular file).
			// That is a policy bug, not a missing feature: continuing would
			// run a step that handles registry secrets with NO filesystem
			// confinement — a silent fail-open. Fail CLOSED instead.
			fmt.Fprintf(os.Stderr, "[sklp] filesystem confinement FAILED to apply (%v); refusing to run unconfined\n", err)
			os.Exit(127)
		}
	}
	// Drop the shim env so it is not visible to the user's command.
	_ = os.Unsetenv(sandboxShimEnv)

	// cmd[0] is the absolute program path the parent already resolved via
	// its own PATH (applyFilesystemIsolation). We must NOT LookPath here:
	// the shim's PATH is the step's composed env (may omit the bin's dir)
	// and Landlock would block a PATH walk. Exec the absolute path.
	bin := cmd[0]
	if !filepath.IsAbs(bin) {
		// Parent could not resolve it; last-resort PATH lookup.
		if p, err := exec.LookPath(bin); err == nil {
			bin = p
		}
	}
	if err := syscall.Exec(bin, cmd, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "[sklp] sandbox shim: exec %s: %v\n", bin, err)
		os.Exit(127)
	}
}

// restrictLandlock builds and applies the ruleset from the policy.
// BestEffort() means older kernels silently apply the subset they
// support (or nothing), so this never errors purely on a missing LSM.
func restrictLandlock(policy []fsAccess) error {
	rules := make([]landlock.Rule, 0, len(policy))
	for _, a := range policy {
		if len(a.Paths) == 0 {
			continue
		}
		switch a.Kind {
		case "rw":
			rules = append(rules, landlock.RWDirs(a.Paths...).IgnoreIfMissing())
		case "ro":
			rules = append(rules, landlock.RODirs(a.Paths...).IgnoreIfMissing())
		case "rofile":
			rules = append(rules, landlock.ROFiles(a.Paths...).IgnoreIfMissing())
		case "rwfile":
			rules = append(rules, landlock.RWFiles(a.Paths...).IgnoreIfMissing())
		default:
			return fmt.Errorf("unknown fs access kind %q", a.Kind)
		}
	}
	if len(rules) == 0 {
		return nil
	}
	return landlock.V5.BestEffort().RestrictPaths(rules...)
}
