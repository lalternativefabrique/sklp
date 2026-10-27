package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeTask drops a minimal runnable pipeline at <repo>/<rel> with the
// given require: block, returning the relative task path.
func writeTask(t *testing.T, repo, rel, requireBody string) string {
	t.Helper()
	body := "name: gated\n" + requireBody + "\nsteps:\n  - name: noop\n    run: \"true\"\n"
	abs := filepath.Join(repo, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatalf("write task: %v", err)
	}
	return rel
}

// Regression (#412 follow-up): the Space execution path must honour the
// require: gate BEFORE any Space/rootfs/build work. Before the fix,
// ParseArgsAndRun reached sp.Spawn (apt/rootfs) on a non-main branch because
// RunPipeline's gate fired only inside the container.
//
// The Space path is the default (no SKLP_RUNNER_MODE → not already in a
// sandbox). On a repo whose branch is NOT in require.branches we assert
// ParseArgsAndRun returns ErrRequireViolation. The only way to get that error
// WITHOUT a real container is the hoisted host-side gate: the spawn path can't
// run here (no runner toolchain in the unit test), so reaching it would
// surface a spawn error, not a require violation.
func TestParseArgsAndRun_RequireGatesContainerPathBeforeSpawn(t *testing.T) {
	t.Setenv(RunnerModeEnv, "") // ensure we take the Space path, not direct.
	repo := initRepo(t, "feat/x")
	task := writeTask(t, repo, ".sklp/pipelines/publish.yaml",
		"require:\n  branches: [main]\n  clean_tree: true")

	_, err := ParseArgsAndRun(context.Background(), []string{
		"-task", task, "-repo", repo,
	})
	if err == nil {
		t.Fatal("expected a require violation, got nil")
	}
	if !errors.Is(err, ErrRequireViolation) {
		t.Fatalf("want ErrRequireViolation (gate fired before spawn), got: %v", err)
	}
}

// evaluateRequireForTask is the host-side gate helper. On a matching
// branch it must return nil (no spawn / build happens); on a mismatch it
// returns a require violation. Testing the helper directly avoids driving
// a real Space spawn from a unit test.
func TestEvaluateRequireForTask(t *testing.T) {
	t.Run("matching branch passes", func(t *testing.T) {
		repo := initRepo(t, "main")
		task := writeTask(t, repo, ".sklp/pipelines/publish.yaml",
			"require:\n  branches: [main]")
		if err := evaluateRequireForTask(context.Background(), task, repo); err != nil {
			t.Fatalf("matching branch should pass, got: %v", err)
		}
	})
	t.Run("mismatched branch is refused", func(t *testing.T) {
		repo := initRepo(t, "feat/x")
		task := writeTask(t, repo, ".sklp/pipelines/publish.yaml",
			"require:\n  branches: [main]")
		err := evaluateRequireForTask(context.Background(), task, repo)
		if !errors.Is(err, ErrRequireViolation) {
			t.Fatalf("want ErrRequireViolation, got: %v", err)
		}
	})
}
