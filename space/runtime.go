// Package runtime is the daemonless OCI backend for sklp's Space engine.
//
// It runs the whole container lifecycle in process: pull the runner image
// from a registry, unpack a rootless rootfs into the user's cache,
// synthesise an OCI runtime-spec (config.json), and invoke `runc run`
// against it. No docker daemon, no podman, no privileged operations.
//
// It is the only CI backend: a Space run routes here via runtime.Run(...).
// The in-process path (when sklp is already running inside the Space)
// bypasses it entirely.
//
// Scope of this first iteration:
//   - Linux only (the package uses Linux-specific syscalls via runc).
//   - runc + slirp4netns are go:embed'd (see runtime/embed) and extracted
//     to the cache at first run; nothing is required on PATH.
//   - Single-container runs only; ci.yaml `services:` sidecars are not
//     yet wired through (the multi-container POC proves the pattern).
//   - Auth from the sklp registries file for private registries.
package space

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	rtembed "github.com/lalternativefabrique/sklp/space/embed"
)

// Config is the input to Run. It mirrors spawn.Config but is kept
// independent so the embedded backend has no circular dependency on
// the outer spawn package.
type Config struct {
	// Image is the OCI reference to pull and run, e.g.
	// "registry.example.com/acme/sklp-runner-base:latest".
	Image string

	// RepoPath is the absolute host path bind-mounted at /workspace
	// inside the container.
	RepoPath string

	// TaskName is the sklp task to run inside the container. Empty
	// falls back to "ci".
	TaskName string

	// ExtraArgs are appended to the inner `sklp run <task> ...` argv,
	// matching the docker backend's contract (-no-pull, -force, ...).
	ExtraArgs []string

	// Env is forwarded into the container's process env, layered on
	// top of the defaults (PATH, HOME=/tmp). KEY=value entries; later
	// duplicates win.
	Env []string

	// LiveOut receives the container's stderr (human progress) live.
	// stdout (the JSON report) is captured and returned separately.
	// nil discards the live stream.
	LiveOut io.Writer

	// CPULimit caps CPU as a number of cores (2.0 = two cores, 0.5 =
	// half a core). 0 means unlimited. Mapped to cgroups v2 cpu.max via
	// quota/period (period fixed at 100ms). Best-effort: applied only
	// when cgroups v2 delegation is available (see cgroups.go).
	CPULimit float64

	// MemoryLimit caps memory in bytes (e.g. 512<<20 for 512 MiB). 0
	// means unlimited. Mapped to cgroups v2 memory.max. Best-effort:
	// applied only when cgroups v2 delegation is available.
	MemoryLimit int64

	// PidsLimit caps the number of processes/threads in the container. 0
	// means unlimited. Mapped to cgroups v2 pids.max — the anti-fork-bomb
	// knob. The pids controller is usually delegated even when cpu/memory
	// are not, so this often applies where the other limits cannot.
	PidsLimit int64

	// RequireLimits turns the cgroup fallback from best-effort into a hard
	// requirement. When false (interactive/dev), a requested limit that
	// cannot be applied (no cgroups v2 delegation) is dropped with a
	// warning and the run proceeds unbounded. When true (CI), the same
	// situation FAILS the run: a requested cap is a contract, and silently
	// running unbounded lets one leaky run OOM the host and kill unrelated
	// runs. Set it from the CI entrypoint.
	RequireLimits bool

	// Services are the pod-like sidecars to launch BEFORE the runner, all
	// sharing one network namespace with it (loopback-only; see the
	// service.go package doc for the sharing model and the Ports/Network
	// limitation). Empty (the default) keeps the single-container
	// behaviour byte-identical: the runner creates its own namespaces.
	Services []Service
}

// Run executes a sklp pipeline inside the embedded runtime and returns
// the JSON report the inner runner emitted on stdout (or whatever it
// produced before failing, when err != nil).
func Run(ctx context.Context, cfg Config) (string, error) {
	if cfg.Image == "" {
		return "", fmt.Errorf("spawner(embedded): Image is required")
	}
	if !filepath.IsAbs(cfg.RepoPath) {
		return "", fmt.Errorf("spawner(embedded): RepoPath must be absolute: %q", cfg.RepoPath)
	}

	// The whole pipeline runs inside ONE runtime.Space: services on the
	// SpaceNet bridge with DNS-by-name + hook-wired egress, a `run:` service on
	// the runner rootfs, and the runner exec'd in. This is the single CI path.
	return runViaSpace(ctx, cfg)
}

// resolveRunc returns the runc binary path the runtime invokes. SKLP_RUNC_PATH
// is an unsafe development override and only works with SKLP_UNSAFE_RUNC_PATH=1;
// otherwise the pinned embedded runc is extracted to the cache.
func resolveRunc(cacheRoot string) (string, error) {
	if p := os.Getenv("SKLP_RUNC_PATH"); p != "" {
		if os.Getenv("SKLP_UNSAFE_RUNC_PATH") != "1" {
			return "", fmt.Errorf("SKLP_RUNC_PATH is disabled unless SKLP_UNSAFE_RUNC_PATH=1 is set")
		}
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("SKLP_RUNC_PATH=%q: path must be absolute", p)
		}
		info, err := os.Stat(p)
		if err != nil {
			return "", fmt.Errorf("SKLP_RUNC_PATH=%q: %w", p, err)
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("SKLP_RUNC_PATH=%q: not a regular file", p)
		}
		if info.Mode().Perm()&0o111 == 0 {
			return "", fmt.Errorf("SKLP_RUNC_PATH=%q: file is not executable", p)
		}
		if info.Mode().Perm()&0o002 != 0 {
			return "", fmt.Errorf("SKLP_RUNC_PATH=%q: refusing world-writable executable", p)
		}
		return p, nil
	}
	return rtembed.ExtractRunc(cacheRoot)
}

// sanitize turns a task name into a safe directory / container id
// suffix. Kept identical to the POC for cache continuity.
func sanitize(s string) string {
	if s == "" {
		return "run"
	}
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, strings.ToLower(s))
}
