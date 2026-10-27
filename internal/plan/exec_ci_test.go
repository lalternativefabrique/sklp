package plan

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestExecuteCI_HappyPath(t *testing.T) {
	pl := &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindCI,
		CI: &CIPlan{
			Steps: []CIStep{
				{Name: "echo", Run: "echo hello-world", Status: StatusIncluded},
				{Name: "math", Run: "echo $((2+2))", Status: StatusIncluded},
			},
		},
	}
	var out bytes.Buffer
	rep, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan:   pl,
		Repo:   t.TempDir(),
		Stdout: &out,
		Stderr: &out,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if rep.PassFail != "pass" {
		t.Errorf("pass_fail = %q, want pass", rep.PassFail)
	}
	if len(rep.Steps) != 2 || rep.Steps[0].Status != "passed" || rep.Steps[1].Status != "passed" {
		t.Errorf("steps wrong: %+v", rep.Steps)
	}
	if !strings.Contains(out.String(), "hello-world") {
		t.Errorf("missing step output: %q", out.String())
	}
}

func TestExecuteCI_FailureStopsRun(t *testing.T) {
	pl := &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindCI,
		CI: &CIPlan{
			Steps: []CIStep{
				{Name: "ok", Run: "true", Status: StatusIncluded},
				{Name: "boom", Run: "exit 17", Status: StatusIncluded, DependsOn: []string{"ok"}},
				{Name: "never", Run: "echo nope", Status: StatusIncluded, DependsOn: []string{"boom"}},
			},
		},
	}
	var out bytes.Buffer
	rep, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan: pl, Repo: t.TempDir(), Stdout: &out, Stderr: &out, Jobs: 1,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if rep.PassFail != "fail" {
		t.Errorf("pass_fail = %q, want fail", rep.PassFail)
	}
	want := []string{"passed", "failed", "skipped"}
	for i, w := range want {
		if rep.Steps[i].Status != w {
			t.Errorf("step[%d] status = %q, want %q", i, rep.Steps[i].Status, w)
		}
	}
	if rep.Steps[1].ExitCode != 17 {
		t.Errorf("exit code = %d, want 17", rep.Steps[1].ExitCode)
	}
}

func TestExecuteCI_CompileSkippedStaysSkipped(t *testing.T) {
	pl := &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindCI,
		CI: &CIPlan{
			Steps: []CIStep{
				{Name: "scoped", Status: StatusSkippedCompile, SkipReason: "no changes in scope"},
				{Name: "after", Run: "true", Status: StatusIncluded},
			},
		},
	}
	rep, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan: pl, Repo: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if rep.Steps[0].Status != "skipped" || rep.Steps[0].SkipReason == "" {
		t.Errorf("compile-skipped step lost reason: %+v", rep.Steps[0])
	}
	if rep.Steps[1].Status != "passed" {
		t.Errorf("subsequent step should pass: %+v", rep.Steps[1])
	}
}

func TestExecuteCI_RejectsWrongKind(t *testing.T) {
	pl := &Plan{SchemaVersion: SchemaVersion, Kind: KindPublish}
	_, err := ExecuteCI(context.Background(), CIExecConfig{Plan: pl, Repo: t.TempDir()})
	if err == nil {
		t.Fatal("expected error for non-CI plan")
	}
}

func TestExecuteCI_ResumeRejectsNoCache(t *testing.T) {
	pl := &Plan{SchemaVersion: SchemaVersion, Kind: KindCI, CI: &CIPlan{}}
	_, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan: pl, Repo: t.TempDir(), Resume: true, NoCache: true,
	})
	if err == nil || !strings.Contains(err.Error(), "Resume") {
		t.Fatalf("expected mutually-exclusive error, got %v", err)
	}
}

func TestExecuteCI_ResumeWithoutPreviousRunIsNoOp(t *testing.T) {
	repo := t.TempDir()
	cache := OpenStepCache(repo)
	pl := newCIPlan("true", "true")
	planKey, _ := ComputeCIPlanKey(pl, "")

	rep, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan: pl, Repo: repo, Cache: cache, PlanKey: planKey, Resume: true,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for i, s := range rep.Steps {
		if s.Status != "passed" {
			t.Errorf("step[%d] status = %q, want passed (resume should be a no-op without a record)", i, s.Status)
		}
	}
}

func TestExecuteCI_ResumeAfterFailureReplaysPriorPasses(t *testing.T) {
	repo := t.TempDir()
	cache := OpenStepCache(repo)

	// First run: A passes, B fails, C is skipped (cascade).
	pl1 := &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindCI,
		CI: &CIPlan{
			Steps: []CIStep{
				{Name: "A", Run: "true", Status: StatusIncluded},
				{Name: "B", Run: "exit 1", Status: StatusIncluded, DependsOn: []string{"A"}},
				{Name: "C", Run: "true", Status: StatusIncluded, DependsOn: []string{"B"}},
			},
		},
	}
	pk1, _ := ComputeCIPlanKey(pl1, "")
	rep1, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan: pl1, Repo: repo, Cache: cache, PlanKey: pk1, Jobs: 1,
	})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if rep1.PassFail != "fail" || rep1.Steps[0].Status != "passed" || rep1.Steps[1].Status != "failed" {
		t.Fatalf("first run wrong shape: %+v", rep1.Steps)
	}

	// Edit B's command. A's key is unchanged so its cache entry
	// still applies; B's key changes so it re-runs.
	pl2 := &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindCI,
		CI: &CIPlan{
			Steps: []CIStep{
				{Name: "A", Run: "true", Status: StatusIncluded},
				{Name: "B", Run: "true", Status: StatusIncluded, DependsOn: []string{"A"}},
				{Name: "C", Run: "true", Status: StatusIncluded, DependsOn: []string{"B"}},
			},
		},
	}
	// For resume to find the prior record, the plan key must match the
	// FIRST run's plan key. We deliberately keep pk1 here: editing B's
	// command would change pk in practice; the spec calls that out as
	// "plan key changed → resume bypassed". This test asserts the
	// happy path where the prior record IS found, i.e. the plan key
	// is the same. We achieve that by saving a synthetic last-run
	// under pk2 with A=passed and rerunning under pk2.
	pk2, _ := ComputeCIPlanKey(pl2, "")
	if err := SaveLastRun(repo, pk2, []CIStepResult{
		{Name: "A", Status: "passed"},
		{Name: "B", Status: "failed"},
		{Name: "C", Status: "skipped"},
	}); err != nil {
		t.Fatalf("seed last-run: %v", err)
	}

	rep2, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan: pl2, Repo: repo, Cache: cache, PlanKey: pk2, Resume: true, Jobs: 1,
	})
	if err != nil {
		t.Fatalf("resume run: %v", err)
	}
	if rep2.Steps[0].Status != "cached" {
		t.Errorf("A should be cached on resume, got %q", rep2.Steps[0].Status)
	}
	if rep2.Steps[1].Status != "passed" {
		t.Errorf("B should re-run and pass, got %q", rep2.Steps[1].Status)
	}
	if rep2.Steps[2].Status != "passed" {
		t.Errorf("C should run after B passes, got %q", rep2.Steps[2].Status)
	}
}

func TestExecuteCI_ResumeGracefulOnEviction(t *testing.T) {
	repo := t.TempDir()
	cache := OpenStepCache(repo)
	pl := newCIPlan("true", "true")
	planKey, _ := ComputeCIPlanKey(pl, "")

	// Seed a last-run that claims both steps passed, but DO NOT
	// populate the per-step cache (simulating eviction).
	if err := SaveLastRun(repo, planKey, []CIStepResult{
		{Name: "first", Status: "passed"},
		{Name: "second", Status: "passed"},
	}); err != nil {
		t.Fatalf("seed last-run: %v", err)
	}

	var stderr bytes.Buffer
	rep, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan: pl, Repo: repo, Cache: cache, PlanKey: planKey, Resume: true,
		Stderr: &stderr, Jobs: 1,
	})
	if err != nil {
		t.Fatalf("resume run: %v", err)
	}
	for i, s := range rep.Steps {
		if s.Status != "passed" {
			t.Errorf("step[%d] status = %q, want passed (graceful re-run on eviction)", i, s.Status)
		}
	}
	if !strings.Contains(stderr.String(), "cache entry missing") {
		t.Errorf("expected eviction warning in stderr, got %q", stderr.String())
	}
}

func TestExecuteCI_LastRunPersistedAfterRun(t *testing.T) {
	repo := t.TempDir()
	cache := OpenStepCache(repo)
	pl := newCIPlan("true", "exit 3")
	planKey, _ := ComputeCIPlanKey(pl, "")

	rep, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan: pl, Repo: repo, Cache: cache, PlanKey: planKey, Jobs: 1,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.PassFail != "fail" {
		t.Fatalf("pass_fail = %q", rep.PassFail)
	}
	loaded, err := LoadLastRun(repo, planKey)
	if err != nil {
		t.Fatalf("load last-run: %v", err)
	}
	if len(loaded.Steps) != 2 || loaded.Steps[0].Status != "passed" || loaded.Steps[1].Status != "failed" {
		t.Errorf("last-run not persisted accurately: %+v", loaded.Steps)
	}
}
