package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/pipeline/spec"
)

func gitStub(branch, status string) requireGitRunner {
	return requireGitRunner{run: func(_ context.Context, _ string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "status" {
			return status, nil
		}
		return branch + "\n", nil
	}}
}

func TestEvaluateTrigger_RefusesBranchOffTheList(t *testing.T) {
	trig := &spec.TriggerBlock{Branches: []string{"main"}}
	err := evaluateTrigger(context.Background(), trig, "/repo", gitStub("feat/x", ""))
	if err == nil {
		t.Fatal("a hand-run from feat/x must be refused when on.branches is [main]")
	}
	var v *RequireViolationError
	if !errors.As(err, &v) {
		t.Fatalf("want a RequireViolationError, got %T", err)
	}
	if !strings.Contains(err.Error(), "feat/x") {
		t.Errorf("the refusal must name the offending branch: %v", err)
	}
}

func TestEvaluateTrigger_AllowsListedBranch(t *testing.T) {
	trig := &spec.TriggerBlock{Branches: []string{"main"}}
	if err := evaluateTrigger(context.Background(), trig, "/repo", gitStub("main", "")); err != nil {
		t.Fatalf("main is listed, the run must proceed: %v", err)
	}
}

func TestEvaluateTrigger_NegationCarvesMainOut(t *testing.T) {
	trig := &spec.TriggerBlock{Branches: []string{"**", "!main"}}
	if err := evaluateTrigger(context.Background(), trig, "/repo", gitStub("feat/x", "")); err != nil {
		t.Fatalf("a CI pipeline must run on a feature branch: %v", err)
	}
	if err := evaluateTrigger(context.Background(), trig, "/repo", gitStub("main", "")); err == nil {
		t.Fatal("['**','!main'] must refuse main on the hand-run path too")
	}
}

func TestEvaluateTrigger_CleanTree(t *testing.T) {
	trig := &spec.TriggerBlock{Branches: []string{"main"}, CleanTree: true}
	dirty := gitStub("main", " M services/api/main.go\n?? scratch.txt\n")
	err := evaluateTrigger(context.Background(), trig, "/repo", dirty)
	if err == nil {
		t.Fatal("a dirty tree must refuse the run")
	}
	if !strings.Contains(err.Error(), "dirty") {
		t.Errorf("the refusal must say the tree is dirty: %v", err)
	}
	if err := evaluateTrigger(context.Background(), trig, "/repo", gitStub("main", "")); err != nil {
		t.Fatalf("a clean tree on main must proceed: %v", err)
	}
}

func TestEvaluateTrigger_NilBlockIsNoOp(t *testing.T) {
	if err := evaluateTrigger(context.Background(), nil, "/repo", gitStub("anything", "")); err != nil {
		t.Fatalf("a pipeline with no on: block runs anywhere: %v", err)
	}
}

func TestEvaluateTrigger_NoBranchesSkipsGit(t *testing.T) {
	called := false
	git := requireGitRunner{run: func(_ context.Context, _ string, _ ...string) (string, error) {
		called = true
		return "", nil
	}}
	trig := &spec.TriggerBlock{Never: true}
	if err := evaluateTrigger(context.Background(), trig, "/repo", git); err != nil {
		t.Fatalf("never: gates selection, not a run named by hand: %v", err)
	}
	if called {
		t.Error("no branch rule to evaluate — git must not be shelled out")
	}
}
