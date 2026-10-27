package devstack

// Run-state persistence for `sklp dev down`.
//
// `sklp dev` is a foreground supervisor: it holds every running
// service as an in-memory *proc and tears them down on Ctrl-C via
// supervisor.stopAll. A SEPARATE `sklp dev down` invocation has no
// access to those handles, so the launching process drops a small
// JSON statefile on disk (one per repo/worktree, keyed by devScopeID)
// recording what it would need to kill: the process-group IDs, the
// OCI container names, and the bridge-network name. `down` reads it
// back and replays the same teardown out-of-process.
//
// The statefile lives under $XDG_CACHE_HOME/sklp/dev/ (~/.cache/sklp/dev/)
// — it is per-user runtime state (PIDs, pgids, a bridge name) that means
// nothing outside this host, so it does NOT belong in the repo tree. It is
// keyed by devScopeID (a hash of the absolute repo path + stack), so two
// worktrees never collide despite sharing the directory. It is best-effort:
// a crashed `sklp dev` (kill -9) leaves a stale file, so `down` treats
// already-dead pgids and missing containers as success.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lalternativefabrique/sklp/internal/xdg"
)

// runState is the on-disk record a running `sklp dev` leaves behind so
// `sklp dev down` can find and kill it. All fields are best-effort:
// down tolerates entries that are already gone.
type runState struct {
	// SupervisorPID is the `sklp dev` process itself. down signals it
	// first (SIGTERM) so a still-running supervisor performs its own
	// graceful shutdown; the pgid/OCI sweep below is the fallback for
	// a supervisor that already died and orphaned its children.
	SupervisorPID int `json:"supervisor_pid"`

	// PGIDs are the process-group IDs of every launched service. Each
	// service runs under Setpgid (see startProc), so signalling -pgid
	// reaches the shell + go run/vite/pnpm subtree in one shot.
	PGIDs []int `json:"pgids"`

	// OCIContainers are the `sklp-dev-<svc>` container names to stop.
	OCIContainers []string `json:"oci_containers,omitempty"`

	// OCIRuntime is the runtime binary name (docker/podman/nerdctl)
	// resolved at launch, so down stops containers with the same tool.
	OCIRuntime string `json:"oci_runtime,omitempty"`

	// Network is the run-scoped bridge network to remove, if any.
	Network string `json:"network,omitempty"`

	// Embedded marks a run launched in the embedded runc runtime. When set,
	// down stops the supervisor (which Closes the launcher: containers +
	// space torn down); EmbeddedSpace lets a fallback reap orphaned
	// containers if the supervisor already died. PGIDs/OCIContainers stay
	// empty for an embedded run — the runc launcher owns the containers.
	Embedded bool `json:"embedded,omitempty"`

	// EmbeddedSpace is the space name the embedded run used, so a dead-
	// supervisor down can reap that space's containers + bridge.
	EmbeddedSpace string `json:"embedded_space,omitempty"`
}

// stateDir is the per-user dev run-state dir, ~/.cache/sklp/dev. It is
// repo-independent: devScopeID folds the repo path into each filename, so
// every worktree's state coexists here without collision and the repo tree
// stays clean (no more <repo>/.sklp/run/).
func stateDir() string {
	return filepath.Join(xdg.CacheRoot(), "dev")
}

// statePath is the per-repo/worktree statefile. devScopeID keys it by
// the absolute repo path (and stack, when set), so two worktrees — or two
// per-stack `sklp dev` runs — get distinct files and `down` in one never
// tears down the other. stack=="" reproduces today's single-stack name.
func statePath(repoAbs, stack string) string {
	return filepath.Join(stateDir(), "dev-"+devScopeID(repoAbs, stack)+".json")
}

// legacyStatePath is the pre-xdg location, <repo>/.sklp/run/dev-<id>.json.
// readState falls back to it so a `sklp dev` launched before this change is
// still findable by a `down` running the new binary. Remove after one release.
func legacyStatePath(repoAbs, stack string) string {
	return filepath.Join(repoAbs, ".sklp", "run", "dev-"+devScopeID(repoAbs, stack)+".json")
}

// writeState persists the run record. Best-effort: a write failure is
// returned so the caller can warn, but it never blocks the launch —
// the only consequence is that `sklp dev down` won't find this run
// (the user can still Ctrl-C the foreground supervisor).
func writeState(repoAbs, stack string, st runState) error {
	dir := stateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	// Atomic-ish write: temp + rename so a concurrent `down` never
	// reads a half-written file.
	tmp := statePath(repoAbs, stack) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, statePath(repoAbs, stack))
}

// readState loads the run record. Returns (nil, nil) when no statefile
// exists — i.e. nothing is running — so callers can distinguish "no
// dev stack" from a real read error.
func readState(repoAbs, stack string) (*runState, error) {
	data, err := os.ReadFile(statePath(repoAbs, stack))
	if os.IsNotExist(err) {
		// Fall back to the pre-xdg <repo>/.sklp/run/ location so a stack
		// launched by an older binary is still found. Drop after one release.
		data, err = os.ReadFile(legacyStatePath(repoAbs, stack))
		if os.IsNotExist(err) {
			return nil, nil
		}
	}
	if err != nil {
		return nil, err
	}
	var st runState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", statePath(repoAbs, stack), err)
	}
	return &st, nil
}

// removeState deletes the statefile. Idempotent: a missing file is
// success (shutdown raced with `down`, or the run already cleaned up).
func removeState(repoAbs, stack string) {
	_ = os.Remove(statePath(repoAbs, stack))
	// Sweep a legacy <repo>/.sklp/run/ file + its now-empty dir, so an
	// upgraded `down` also cleans up after a pre-xdg launch. os.Remove on a
	// non-empty dir fails harmlessly (another stack still owns it).
	_ = os.Remove(legacyStatePath(repoAbs, stack))
	_ = os.Remove(filepath.Join(repoAbs, ".sklp", "run"))
}
