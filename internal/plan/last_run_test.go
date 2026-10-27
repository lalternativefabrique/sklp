package plan

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveAndLoadLastRun(t *testing.T) {
	repo := t.TempDir()
	steps := []CIStepResult{
		{Name: "a", Status: "passed"},
		{Name: "b", Status: "failed"},
		{Name: "c", Status: "skipped"},
	}
	if err := SaveLastRun(repo, "plan-key-1", steps); err != nil {
		t.Fatalf("save: %v", err)
	}
	rec, err := LoadLastRun(repo, "plan-key-1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if rec.PlanKey != "plan-key-1" {
		t.Errorf("plan key = %q, want plan-key-1", rec.PlanKey)
	}
	if len(rec.Steps) != 3 {
		t.Fatalf("steps len = %d, want 3", len(rec.Steps))
	}
	want := []string{"passed", "failed", "skipped"}
	for i, s := range rec.Steps {
		if s.Status != want[i] {
			t.Errorf("step[%d].Status = %q, want %q", i, s.Status, want[i])
		}
	}
}

func TestLoadLastRun_MissReturnsSentinel(t *testing.T) {
	repo := t.TempDir()
	_, err := LoadLastRun(repo, "absent-key")
	if !errors.Is(err, ErrLastRunMiss) {
		t.Fatalf("err = %v, want ErrLastRunMiss", err)
	}
}

func TestLoadLastRun_DifferentPlanKeyMisses(t *testing.T) {
	repo := t.TempDir()
	if err := SaveLastRun(repo, "key-A", []CIStepResult{{Name: "x", Status: "passed"}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := LoadLastRun(repo, "key-B"); !errors.Is(err, ErrLastRunMiss) {
		t.Errorf("expected miss for different plan key, got %v", err)
	}
}

func TestLoadLastRun_CorruptJSONMisses(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, ".sklp", "cache", "plans", "k")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "last.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadLastRun(repo, "k"); !errors.Is(err, ErrLastRunMiss) {
		t.Errorf("expected miss for corrupt json, got %v", err)
	}
}
