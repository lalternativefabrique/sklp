package pipeline

import (
	"testing"

	"github.com/lalternativefabrique/sklp/internal/plan"
)

func TestPlanResumeStatus(t *testing.T) {
	repo := t.TempDir()

	// No plans/ directory yet.
	if got := planResumeStatus(repo, "k1"); got != resumeStatusFirstRun {
		t.Errorf("empty repo: got %v, want resumeStatusFirstRun", got)
	}

	// Seed a record under a different plan key.
	if err := plan.SaveLastRun(repo, "old-key", []plan.CIStepResult{{Name: "a", Status: "passed"}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := planResumeStatus(repo, "current-key"); got != resumeStatusKeyChanged {
		t.Errorf("after edit: got %v, want resumeStatusKeyChanged", got)
	}

	// Seed the current key as well.
	if err := plan.SaveLastRun(repo, "current-key", []plan.CIStepResult{{Name: "a", Status: "passed"}}); err != nil {
		t.Fatalf("seed current: %v", err)
	}
	if got := planResumeStatus(repo, "current-key"); got != resumeStatusFound {
		t.Errorf("matching key: got %v, want resumeStatusFound", got)
	}
}
