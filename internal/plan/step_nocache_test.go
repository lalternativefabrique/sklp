package plan

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A step declaring `cache: false` must never be SERVED from the cache, even
// when a matching entry exists. This is the migrate case: the entry was stored
// by an earlier run, but the database it mutated has moved on, so replaying the
// recorded success would skip the schema reset and leave later steps running
// against state nobody applied.
func TestExecuteCI_StepNoCache_IgnoresSeededEntry(t *testing.T) {
	repo := t.TempDir()
	cache := OpenStepCache(repo)
	pl := newCIPlan("true")
	pl.CI.Steps[0].NoCache = true
	planKey, _ := ComputeCIPlanKey(pl, "")

	stepKey, _ := ComputeCIStepKey(pl.CI.Steps[0], pl.CI.Tools, "", "")
	if err := cache.StoreStep(planKey, stepKey, CIStepCacheResult{
		Name: "first", ExitCode: 0, Status: "passed", DurationMS: 1,
	}, []byte("cached-out"), nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var out bytes.Buffer
	rep, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan: pl, Repo: repo, Stdout: &out, Stderr: &out,
		Cache: cache, PlanKey: planKey,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Steps[0].Status != "passed" {
		t.Fatalf("status = %q, want passed (cache must not be consulted)", rep.Steps[0].Status)
	}
	if strings.Contains(out.String(), "cached-out") {
		t.Fatalf("replayed seeded cache bytes: %q", out.String())
	}
}

// Opting out must also block the STORE side. If the result were written, the
// step would be immune on the first run but replayable on the next one — the
// bug would simply move one run later.
func TestExecuteCI_StepNoCache_DoesNotStoreResult(t *testing.T) {
	repo := t.TempDir()
	cache := OpenStepCache(repo)
	pl := newCIPlan("true")
	pl.CI.Steps[0].NoCache = true
	planKey, _ := ComputeCIPlanKey(pl, "")

	var out bytes.Buffer
	if _, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan: pl, Repo: repo, Stdout: &out, Stderr: &out,
		Cache: cache, PlanKey: planKey,
	}); err != nil {
		t.Fatalf("first run: %v", err)
	}

	stepKey, _ := ComputeCIStepKey(pl.CI.Steps[0], pl.CI.Tools, "", "")
	if _, _, _, err := cache.LookupStep(planKey, stepKey); err == nil {
		t.Fatal("opted-out step was persisted to the cache")
	}
}

// The opt-out is PER STEP: a cacheable neighbour in the same plan must keep
// its cache hit. Otherwise declaring `cache: false` on migrate would silently
// de-cache the expensive test step that depends on it.
func TestExecuteCI_StepNoCache_LeavesSiblingsCacheable(t *testing.T) {
	repo := t.TempDir()
	cache := OpenStepCache(repo)
	pl := newCIPlan("true", "true")
	pl.CI.Steps[0].NoCache = true // first opted out, second cacheable
	planKey, _ := ComputeCIPlanKey(pl, "")

	var out1 bytes.Buffer
	if _, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan: pl, Repo: repo, Stdout: &out1, Stderr: &out1,
		Cache: cache, PlanKey: planKey,
	}); err != nil {
		t.Fatalf("first run: %v", err)
	}

	var out2 bytes.Buffer
	rep2, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan: pl, Repo: repo, Stdout: &out2, Stderr: &out2,
		Cache: cache, PlanKey: planKey,
	})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if rep2.Steps[0].Status != "passed" {
		t.Fatalf("opted-out step[0] = %q, want passed on every run", rep2.Steps[0].Status)
	}
	if rep2.Steps[1].Status != "cached" {
		t.Fatalf("sibling step[1] = %q, want cached (opt-out must not spread)", rep2.Steps[1].Status)
	}
}

// The mutation an opted-out step performs must actually happen on every run.
// Status alone could be reported "passed" while the command was skipped, so
// assert on a real side effect: the step appends to a file each time it runs.
func TestExecuteCI_StepNoCache_SideEffectRepeats(t *testing.T) {
	repo := t.TempDir()
	cache := OpenStepCache(repo)
	marker := filepath.Join(repo, "runs.txt")
	pl := newCIPlan("echo run >> " + marker)
	pl.CI.Steps[0].NoCache = true
	planKey, _ := ComputeCIPlanKey(pl, "")

	for i := range 2 {
		var out bytes.Buffer
		if _, err := ExecuteCI(context.Background(), CIExecConfig{
			Plan: pl, Repo: repo, Stdout: &out, Stderr: &out,
			Cache: cache, PlanKey: planKey,
		}); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}

	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if got := strings.Count(string(b), "run"); got != 2 {
		t.Fatalf("side effect ran %d time(s), want 2 — a cache replay skipped the mutation", got)
	}
}

// Sanity: without the opt-out the same plan DOES cache, so the tests above
// prove the flag's effect and not merely a cache that never worked.
func TestExecuteCI_WithoutOptOut_StillCaches(t *testing.T) {
	repo := t.TempDir()
	cache := OpenStepCache(repo)
	pl := newCIPlan("true")
	planKey, _ := ComputeCIPlanKey(pl, "")

	var out1 bytes.Buffer
	if _, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan: pl, Repo: repo, Stdout: &out1, Stderr: &out1,
		Cache: cache, PlanKey: planKey,
	}); err != nil {
		t.Fatalf("first run: %v", err)
	}

	var out2 bytes.Buffer
	rep2, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan: pl, Repo: repo, Stdout: &out2, Stderr: &out2,
		Cache: cache, PlanKey: planKey,
	})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if rep2.Steps[0].Status != "cached" {
		t.Fatalf("control: status = %q, want cached", rep2.Steps[0].Status)
	}
}
