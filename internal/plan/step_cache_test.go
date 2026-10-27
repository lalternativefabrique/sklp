package plan

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newCIPlan builds a minimal Plan with the given step Runs.
func newCIPlan(runs ...string) *Plan {
	steps := make([]CIStep, 0, len(runs))
	for i, r := range runs {
		steps = append(steps, CIStep{
			Name:   nameForIdx(i),
			Run:    r,
			Status: StatusIncluded,
		})
	}
	return &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindCI,
		CI:            &CIPlan{Steps: steps},
	}
}

func nameForIdx(i int) string {
	switch i {
	case 0:
		return "first"
	case 1:
		return "second"
	case 2:
		return "third"
	}
	return "stepN"
}

func TestStepCache_HitReplaysStreams(t *testing.T) {
	repo := t.TempDir()
	cache := OpenStepCache(repo)

	res := CIStepCacheResult{
		Name:       "echo",
		ExitCode:   0,
		Status:     "passed",
		DurationMS: 42,
	}
	if err := cache.StoreStep("planK", "stepK", res, []byte("hello\n"), []byte("warn\n")); err != nil {
		t.Fatalf("store: %v", err)
	}
	got, sout, serr, err := cache.LookupStep("planK", "stepK")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.ExitCode != 0 || got.Status != "passed" || got.DurationMS != 42 {
		t.Fatalf("result drift: %+v", got)
	}
	if string(sout) != "hello\n" || string(serr) != "warn\n" {
		t.Fatalf("stream drift: stdout=%q stderr=%q", sout, serr)
	}
}

func TestStepCache_MissReturnsSentinel(t *testing.T) {
	cache := OpenStepCache(t.TempDir())
	_, _, _, err := cache.LookupStep("planK", "missing")
	if !errors.Is(err, ErrStepCacheMiss) {
		t.Fatalf("want ErrStepCacheMiss, got %v", err)
	}
}

func TestExecuteCI_CacheMissThenHit(t *testing.T) {
	repo := t.TempDir()
	cache := OpenStepCache(repo)

	pl := newCIPlan("printf 'one\\n'", "printf 'two\\n'")
	planKey, err := ComputeCIPlanKey(pl, "")
	if err != nil {
		t.Fatalf("plan key: %v", err)
	}

	// First run — cache miss, should spawn shells and store results.
	var out1 bytes.Buffer
	rep1, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan:    pl,
		Repo:    repo,
		Stdout:  &out1,
		Stderr:  &out1,
		Cache:   cache,
		PlanKey: planKey,
	})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if rep1.PassFail != "pass" {
		t.Fatalf("first run pass_fail = %q", rep1.PassFail)
	}
	for i, s := range rep1.Steps {
		if s.Status != "passed" {
			t.Fatalf("first run step[%d] status = %q, want passed", i, s.Status)
		}
	}
	if !strings.Contains(out1.String(), "one") || !strings.Contains(out1.String(), "two") {
		t.Fatalf("first run did not produce step output: %q", out1.String())
	}

	// Second run — every step must hit the cache. We assert this by
	// using a Run that, if it actually executed again, would FAIL the
	// step status check (we leave the plan unchanged so the keys match,
	// but the assertion is on Status == "cached" which only the cache
	// path emits).
	var out2 bytes.Buffer
	rep2, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan:    pl,
		Repo:    repo,
		Stdout:  &out2,
		Stderr:  &out2,
		Cache:   cache,
		PlanKey: planKey,
	})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	for i, s := range rep2.Steps {
		if s.Status != "cached" {
			t.Fatalf("second run step[%d] status = %q, want cached", i, s.Status)
		}
	}
	if !strings.Contains(out2.String(), "one") || !strings.Contains(out2.String(), "two") {
		t.Fatalf("second run did not replay step output: %q", out2.String())
	}
}

func TestExecuteCI_NoCacheBypass(t *testing.T) {
	repo := t.TempDir()
	cache := OpenStepCache(repo)
	pl := newCIPlan("true")
	planKey, _ := ComputeCIPlanKey(pl, "")

	// Seed the cache with a known result so we can distinguish a
	// bypass (status "passed") from a hit (status "cached").
	stepKey, _ := ComputeCIStepKey(pl.CI.Steps[0], pl.CI.Tools, "", "")
	if err := cache.StoreStep(planKey, stepKey, CIStepCacheResult{
		Name: "first", ExitCode: 0, Status: "passed", DurationMS: 1,
	}, []byte("cached-out"), nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var out bytes.Buffer
	rep, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan:    pl,
		Repo:    repo,
		Stdout:  &out,
		Stderr:  &out,
		Cache:   cache,
		PlanKey: planKey,
		NoCache: true, // <— the bypass
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Steps[0].Status != "passed" {
		t.Fatalf("NoCache bypass: status = %q, want passed (cache should NOT be consulted)", rep.Steps[0].Status)
	}
	if strings.Contains(out.String(), "cached-out") {
		t.Fatalf("NoCache bypass replayed seeded cached bytes: %q", out.String())
	}
}

func TestExecuteCI_CommandChangeInvalidatesDownstream(t *testing.T) {
	repo := t.TempDir()
	cache := OpenStepCache(repo)

	// First plan: A -> B. Run, cache both.
	pl1 := newCIPlan("true", "true")
	pk1, _ := ComputeCIPlanKey(pl1, "")
	if _, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan: pl1, Repo: repo, Cache: cache, PlanKey: pk1,
	}); err != nil {
		t.Fatalf("first: %v", err)
	}

	// Second plan: A' (changed Run) -> B (unchanged source). Even
	// though B's literal Run is identical, its upstream key changes
	// because A changed, so B must invalidate too.
	pl2 := newCIPlan("printf changed", "true")
	pk2, _ := ComputeCIPlanKey(pl2, "")
	rep, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan: pl2, Repo: repo, Cache: cache, PlanKey: pk2,
	})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if rep.Steps[0].Status != "passed" {
		t.Fatalf("step A (changed) should run, got %q", rep.Steps[0].Status)
	}
	if rep.Steps[1].Status != "passed" {
		t.Fatalf("step B (downstream) should also run after A changed, got %q (cache invalidation chain broken)", rep.Steps[1].Status)
	}
}

// TestExecuteCI_OutputRestoredFromCAS validates engine 2.4 acceptance
// #3: a declared output captured on the first run is restored from the
// CAS on the second (cached) run even after the working-tree copy was
// deleted.
func TestExecuteCI_OutputRestoredFromCAS(t *testing.T) {
	repo := t.TempDir()
	cache := OpenStepCache(repo)
	cas := OpenCAS(repo)

	step := CIStep{
		Name:    "produce",
		Run:     "printf 'foo\\n' > out.txt",
		Status:  StatusIncluded,
		Outputs: []string{"out.txt"},
	}
	pl := &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindCI,
		CI:            &CIPlan{Steps: []CIStep{step}},
	}
	planKey, _ := ComputeCIPlanKey(pl, repo)

	rep1, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan: pl, Repo: repo, Cache: cache, CAS: cas, PlanKey: planKey,
	})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if rep1.Steps[0].Status != "passed" {
		t.Fatalf("first run status = %q", rep1.Steps[0].Status)
	}
	produced := filepath.Join(repo, "out.txt")
	if got, err := os.ReadFile(produced); err != nil || string(got) != "foo\n" {
		t.Fatalf("first run did not produce out.txt: data=%q err=%v", got, err)
	}

	// Wipe the working-tree output to simulate `rm out.txt` between runs.
	if err := os.Remove(produced); err != nil {
		t.Fatalf("rm out.txt: %v", err)
	}

	rep2, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan: pl, Repo: repo, Cache: cache, CAS: cas, PlanKey: planKey,
	})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if rep2.Steps[0].Status != "cached" {
		t.Fatalf("second run status = %q, want cached", rep2.Steps[0].Status)
	}
	got, err := os.ReadFile(produced)
	if err != nil {
		t.Fatalf("out.txt was not restored: %v", err)
	}
	if string(got) != "foo\n" {
		t.Fatalf("restored content drift: %q", got)
	}
}

// TestExecuteCI_InputChangeInvalidatesStep covers acceptance #1+#2: an
// edit to a declared input invalidates the step; an edit to an unrelated
// file does NOT.
func TestExecuteCI_InputChangeInvalidatesStep(t *testing.T) {
	repo := t.TempDir()
	cache := OpenStepCache(repo)
	cas := OpenCAS(repo)

	tracked := filepath.Join(repo, "tracked.txt")
	other := filepath.Join(repo, "other.txt")
	if err := os.WriteFile(tracked, []byte("v1"), 0o600); err != nil {
		t.Fatalf("seed tracked: %v", err)
	}
	if err := os.WriteFile(other, []byte("ignored"), 0o600); err != nil {
		t.Fatalf("seed other: %v", err)
	}

	build := func() *Plan {
		return &Plan{
			SchemaVersion: SchemaVersion,
			Kind:          KindCI,
			CI: &CIPlan{Steps: []CIStep{{
				Name:           "read",
				Run:            "cat tracked.txt",
				Status:         StatusIncluded,
				ResolvedInputs: []string{"tracked.txt"},
			}}},
		}
	}

	run := func(pl *Plan) string {
		pk, _ := ComputeCIPlanKey(pl, repo)
		rep, err := ExecuteCI(context.Background(), CIExecConfig{
			Plan: pl, Repo: repo, Cache: cache, CAS: cas, PlanKey: pk,
		})
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		return rep.Steps[0].Status
	}

	if s := run(build()); s != "passed" {
		t.Fatalf("first run status = %q", s)
	}

	// Edit an UNRELATED file — must still hit the cache.
	if err := os.WriteFile(other, []byte("noise"), 0o600); err != nil {
		t.Fatalf("touch other: %v", err)
	}
	if s := run(build()); s != "cached" {
		t.Fatalf("unrelated edit invalidated cache: status = %q, want cached", s)
	}

	// Edit the declared input — must invalidate the step.
	if err := os.WriteFile(tracked, []byte("v2"), 0o600); err != nil {
		t.Fatalf("touch tracked: %v", err)
	}
	if s := run(build()); s != "passed" {
		t.Fatalf("declared-input edit did not invalidate cache: status = %q, want passed", s)
	}
}
