package plan

// Last-run record layout (engine 2.7 — issue #390).
//
// After every ExecuteCI invocation (pass or fail), the executor
// persists the per-step status array of the run under:
//
//	<repo>/.sklp/cache/plans/<plan-key>/last.json
//
// The directory is namespaced by plan key (the same value that
// ComputeCIPlanKey produces), which is intentionally a sibling of the
// per-step cache at .sklp/cache/runs/<step-key>/. Plan-keyed records
// and step-keyed records live in different subtrees, so there is no
// risk of accidental collision and either tree can be wiped without
// affecting the other.
//
// The record is consumed by `sklp run --resume`: each step whose
// previous status was `passed` or `cached` is short-circuited via the
// existing per-step cache; the first step with a non-success previous
// status falls through to normal execution and downstream steps are
// always executed normally regardless of their prior status. The
// record is intentionally minimal — it carries only what the resume
// short-circuit needs to make that decision.
//
// A plan-key mismatch (schema bump, command edit, env change anywhere
// in the pipeline) makes the lookup miss naturally: each Plan compiles
// to a different key, so resume falls back to "run normally" without
// any explicit version check here.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// LastRunStepStatus is one entry of a persisted last-run record.
// Status mirrors CIStepResult.Status ("passed" | "failed" | "cached" |
// "skipped"). Carried as a string (not a typed enum) so the on-disk
// format stays stable across executor refactors.
type LastRunStepStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// LastRunRecord is the JSON shape persisted at last.json.
type LastRunRecord struct {
	PlanKey   string              `json:"plan_key"`
	CreatedAt time.Time           `json:"created_at"`
	Steps     []LastRunStepStatus `json:"steps"`
}

// ErrLastRunMiss is returned by LoadLastRun when no record exists for
// the given plan key. A miss is the resume short-circuit's normal
// "fall back to a full run" signal — not an error worth surfacing.
var ErrLastRunMiss = errors.New("no previous run record for this plan key")

// lastRunDir returns the directory holding last.json for one plan key.
func lastRunDir(repoAbs, planKey string) string {
	return filepath.Join(repoAbs, ".sklp", "cache", "plans", planKey)
}

// SaveLastRun writes the per-step status array for one run. Errors are
// non-fatal at the call-site; the caller logs them but does not fail
// the run, mirroring StoreStep's behaviour on the per-step cache path.
func SaveLastRun(repoAbs, planKey string, steps []CIStepResult) error {
	if planKey == "" {
		return errors.New("SaveLastRun: empty plan key")
	}
	dir := lastRunDir(repoAbs, planKey)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir last-run: %w", err)
	}
	rec := LastRunRecord{
		PlanKey:   planKey,
		CreatedAt: time.Now().UTC(),
		Steps:     make([]LastRunStepStatus, 0, len(steps)),
	}
	for _, s := range steps {
		rec.Steps = append(rec.Steps, LastRunStepStatus{Name: s.Name, Status: s.Status})
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal last-run: %w", err)
	}
	if err := writeAtomic(filepath.Join(dir, "last.json"), b, 0o600); err != nil {
		return fmt.Errorf("write last-run: %w", err)
	}
	return nil
}

// LoadLastRun reads the persisted last-run record for one plan key.
// Returns ErrLastRunMiss on any missing-file / unreadable-JSON
// condition; callers handle every miss identically by running normally.
func LoadLastRun(repoAbs, planKey string) (*LastRunRecord, error) {
	if planKey == "" {
		return nil, ErrLastRunMiss
	}
	path := filepath.Join(lastRunDir(repoAbs, planKey), "last.json")
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrLastRunMiss
		}
		return nil, fmt.Errorf("read last-run: %w", err)
	}
	var rec LastRunRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, ErrLastRunMiss
	}
	return &rec, nil
}
