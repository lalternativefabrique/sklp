package plan

// CI step cache layout (engine 2.3 — issue #386).
//
// The cache lives inside the repository tree at:
//
//	<repo>/.sklp/cache/runs/<step-key>/
//	    result.json    — CIStepCacheResult JSON
//	    stdout.log     — raw bytes captured from sh -c's stdout
//	    stderr.log     — raw bytes captured from sh -c's stderr
//
// <step-key> is ComputeCIStepKey for the individual step — a hex
// SHA-256 of (name, run, cwd, env, tools+versions, inputs-digest
// placeholder, upstream-step-key). It is filename-safe on every
// supported filesystem and already encodes everything the executor
// observes about a step.
//
// Note the lookup is keyed by step-key ALONE, deliberately: editing
// one step changes its own key (and every downstream step's key via
// the UpstreamKey chain), but leaves unrelated steps' keys intact.
// Bucketing by a coarser plan-level key would invalidate every cached
// step whenever any single step changed, breaking the "modify one
// step, only its descendants re-run" acceptance criterion.
//
// On cache hit the executor (exec_ci.go) replays stdout.log / stderr.log
// into the configured writers and records the step as status "cached"
// without spawning a process. On miss it runs the step normally then
// persists the captured streams + result under this layout for the
// next run.
//
// The cache is repo-local on purpose: the values it stores (logs, exit
// codes) only make sense in the context of one checkout. Pruning is
// trivial — delete .sklp/cache/. There is no signing here because the
// host repo is already trusted to the developer's level; the security
// model is "anything inside .sklp/ is yours to corrupt".

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// CIStepCacheResult is the per-step sidecar persisted alongside the
// captured stdout / stderr. Carries just enough information to replay
// the run without spawning sh.
type CIStepCacheResult struct {
	// Name is the step name. Carried for human inspection only; the
	// authoritative key is the directory name.
	Name string `json:"name"`

	// ExitCode is the process exit code from the original run.
	ExitCode int `json:"exit_code"`

	// Status is "passed" or "failed" — we only cache terminal results
	// of an actual run; "skipped" results are not cached because they
	// carry no useful output to replay.
	Status string `json:"status"`

	// DurationMS is the wall-clock duration of the original run, in
	// milliseconds. Surfaced on cache hit so the user sees the saved
	// time, not a misleading "0ms".
	DurationMS int64 `json:"duration_ms"`

	// CreatedAt is when this entry was written, for debugging.
	CreatedAt time.Time `json:"created_at"`

	// Outputs maps the step's declared output paths (repo-relative)
	// to their CAS digests at the time of capture. On a cache hit the
	// executor restores each path from the CAS back into the working
	// tree before downstream steps run. Empty for steps that declare
	// no outputs or whose declared outputs were missing on disk.
	Outputs []CIStepCacheOutput `json:"outputs,omitempty"`
}

// CIStepCacheOutput pairs one declared output path with the digest
// under which its content lives in the CAS.
type CIStepCacheOutput struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

// StepCache is the per-repo on-disk CI step cache.
type StepCache struct {
	// Root is the .sklp/cache/runs directory under the repo. Concrete
	// per-step paths live at Root/<plan-key>/<step-key>/.
	Root string
}

// OpenStepCache returns a StepCache rooted at <repo>/.sklp/cache/runs.
// The directory is created lazily by StoreStep; OpenStepCache itself
// never touches the filesystem.
func OpenStepCache(repoAbs string) *StepCache {
	return &StepCache{Root: filepath.Join(repoAbs, ".sklp", "cache", "runs")}
}

// ErrStepCacheMiss is returned by LookupStep when no usable cache
// entry exists for (planKey, stepKey).
var ErrStepCacheMiss = errors.New("ci step cache miss")

// LookupStep loads the cached result + captured streams for one step.
// Returns ErrStepCacheMiss on any failure mode (missing dir, missing
// file, bad JSON). Callers handle every miss identically: run the step
// and StoreStep afterwards.
//
// The planKey parameter is retained for API symmetry with StoreStep
// (and to keep the door open for a future namespacing scheme), but
// the on-disk layout is currently keyed by stepKey alone — see the
// comment at the top of this file.
func (c *StepCache) LookupStep(planKey, stepKey string) (*CIStepCacheResult, []byte, []byte, error) {
	_ = planKey
	dir := c.stepDir(stepKey)
	resBytes, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil, ErrStepCacheMiss
		}
		return nil, nil, nil, fmt.Errorf("read result: %w", err)
	}
	var res CIStepCacheResult
	if err := json.Unmarshal(resBytes, &res); err != nil {
		return nil, nil, nil, ErrStepCacheMiss
	}
	stdout, err := os.ReadFile(filepath.Join(dir, "stdout.log"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil, fmt.Errorf("read stdout: %w", err)
	}
	stderr, err := os.ReadFile(filepath.Join(dir, "stderr.log"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil, fmt.Errorf("read stderr: %w", err)
	}
	return &res, stdout, stderr, nil
}

// StoreStep persists a step's result + captured streams under
// (planKey, stepKey). Existing entries are overwritten atomically.
func (c *StepCache) StoreStep(planKey, stepKey string, res CIStepCacheResult, stdout, stderr []byte) error {
	_ = planKey
	dir := c.stepDir(stepKey)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir step cache: %w", err)
	}
	res.CreatedAt = time.Now().UTC()
	resBytes, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal result: %w", err)
	}
	if err := writeAtomic(filepath.Join(dir, "result.json"), resBytes, 0o600); err != nil {
		return fmt.Errorf("write result: %w", err)
	}
	if err := writeAtomic(filepath.Join(dir, "stdout.log"), stdout, 0o600); err != nil {
		return fmt.Errorf("write stdout: %w", err)
	}
	if err := writeAtomic(filepath.Join(dir, "stderr.log"), stderr, 0o600); err != nil {
		return fmt.Errorf("write stderr: %w", err)
	}
	return nil
}

// ForgetStep removes one cached step entry. Used by tests; the
// production path relies on key invalidation instead.
func (c *StepCache) ForgetStep(planKey, stepKey string) error {
	_ = planKey
	return os.RemoveAll(c.stepDir(stepKey))
}

// Replay writes the cached stdout / stderr bytes to the supplied
// writers, preserving the original stream split. Convenience used by
// the executor on cache hit.
func Replay(stdoutBytes, stderrBytes []byte, stdout, stderr io.Writer) error {
	if stdout != nil && len(stdoutBytes) > 0 {
		if _, err := stdout.Write(stdoutBytes); err != nil {
			return err
		}
	}
	if stderr != nil && len(stderrBytes) > 0 {
		if _, err := stderr.Write(stderrBytes); err != nil {
			return err
		}
	}
	return nil
}

func (c *StepCache) stepDir(stepKey string) string {
	return filepath.Join(c.Root, stepKey)
}

// writeAtomic creates a temp file in the same directory then renames
// into place, so readers never see a partially-written file even when
// sklp is invoked from multiple shells at once.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".sklp-cache-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
