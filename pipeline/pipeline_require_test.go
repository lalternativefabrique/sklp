package pipeline

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initRepo creates a fresh git repo at dir with a single commit on the
// given branch. Returns the absolute path. Skips the test if `git` is
// not on PATH so CI environments without git stay green.
func initRepo(t *testing.T, branch string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available on PATH")
	}
	dir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	runGit("init", "-q", "-b", branch)
	if err := os.WriteFile(filepath.Join(dir, "seed"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	runGit("add", "seed")
	runGit("commit", "-q", "-m", "seed")
	return dir
}

func TestEvaluateRequire_NilBlockIsNoop(t *testing.T) {
	if err := EvaluateRequire(context.Background(), nil, "/does/not/exist"); err != nil {
		t.Fatalf("nil block should be a no-op, got: %v", err)
	}
}

func TestEvaluateRequire_BranchMatch(t *testing.T) {
	dir := initRepo(t, "main")
	err := EvaluateRequire(context.Background(),
		&RequireBlock{Branches: []string{"main"}}, dir)
	if err != nil {
		t.Fatalf("expected pass, got: %v", err)
	}
}

func TestEvaluateRequire_BranchMismatch(t *testing.T) {
	dir := initRepo(t, "feat/foo")
	err := EvaluateRequire(context.Background(),
		&RequireBlock{Branches: []string{"main"}}, dir)
	if err == nil {
		t.Fatal("expected RequireViolation, got nil")
	}
	if !errors.Is(err, ErrRequireViolation) {
		t.Errorf("expected ErrRequireViolation, got: %v", err)
	}
	want := "refusing to run: branch 'feat/foo' not in require.branches [main]"
	if err.Error() != want {
		t.Errorf("message:\n  got:  %s\n  want: %s", err.Error(), want)
	}
}

func TestEvaluateRequire_CleanTreePasses(t *testing.T) {
	dir := initRepo(t, "main")
	err := EvaluateRequire(context.Background(),
		&RequireBlock{CleanTree: true}, dir)
	if err != nil {
		t.Fatalf("clean tree should pass, got: %v", err)
	}
}

func TestEvaluateRequire_DirtyTreeFails(t *testing.T) {
	dir := initRepo(t, "main")
	// Modify the tracked file (1 unstaged) and add an untracked one.
	if err := os.WriteFile(filepath.Join(dir, "seed"), []byte("changed"), 0o644); err != nil {
		t.Fatalf("modify seed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new"), 0o644); err != nil {
		t.Fatalf("write new: %v", err)
	}
	err := EvaluateRequire(context.Background(),
		&RequireBlock{CleanTree: true}, dir)
	if err == nil {
		t.Fatal("expected RequireViolation, got nil")
	}
	if !errors.Is(err, ErrRequireViolation) {
		t.Errorf("expected ErrRequireViolation, got: %v", err)
	}
	want := "refusing to run: working tree dirty (1 unstaged, 1 untracked)"
	if err.Error() != want {
		t.Errorf("message:\n  got:  %s\n  want: %s", err.Error(), want)
	}
}

func TestEvaluateRequire_BranchCheckedBeforeTree(t *testing.T) {
	// On a mismatched branch with a dirty tree, the branch error must
	// surface first (the user fixes one thing at a time, predictably).
	dir := initRepo(t, "feat/foo")
	if err := os.WriteFile(filepath.Join(dir, "dirt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write dirt: %v", err)
	}
	err := EvaluateRequire(context.Background(),
		&RequireBlock{Branches: []string{"main"}, CleanTree: true}, dir)
	if err == nil || !strings.Contains(err.Error(), "branch 'feat/foo'") {
		t.Fatalf("expected branch-first failure, got: %v", err)
	}
}

// TestRunPipeline_RequireRefusalBeforeCompile asserts the load-bearing
// invariant: a require violation fails the run BEFORE any step runs.
// The pipeline includes a side-effect step that would create a marker
// file if compiled/executed; the marker must NOT appear.
func TestRunPipeline_RequireRefusalBeforeCompile(t *testing.T) {
	dir := initRepo(t, "feat/foo")
	marker := filepath.Join(dir, "step-ran.marker")

	yaml := `
version: 1
kind: pipeline
require:
  branches: [main]
steps:
  - name: side-effect
    run: touch ` + marker + `
`
	path := filepath.Join(dir, "ci.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	p, err := LoadPipeline(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	var stdout, stderr bytes.Buffer
	res, runErr := RunPipeline(context.Background(), p, PipelineRunOptions{
		RepoPath: dir,
		Stdout:   &stdout,
		Stderr:   &stderr,
	})
	if runErr == nil {
		t.Fatal("expected require violation, got nil error")
	}
	if !errors.Is(runErr, ErrRequireViolation) {
		t.Errorf("expected ErrRequireViolation, got: %v", runErr)
	}
	if res.PassFail != "fail" {
		t.Errorf("PassFail = %q, want fail", res.PassFail)
	}
	if len(res.Steps) != 0 {
		t.Errorf("expected 0 step results (no compile), got %d", len(res.Steps))
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("side-effect step ran despite require violation")
	}
}

func TestRunPipeline_RequirePassesOnMatch(t *testing.T) {
	dir := initRepo(t, "main")
	yaml := `
version: 1
kind: pipeline
require:
  branches: [main]
  clean_tree: true
steps:
  - name: noop
    run: "true"
`
	// Write the yaml OUTSIDE the repo so it doesn't make the tree
	// dirty — the test target is RunPipeline's RepoPath, not where
	// the YAML happens to live on disk.
	path := filepath.Join(t.TempDir(), "ci.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	p, err := LoadPipeline(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var stdout, stderr bytes.Buffer
	res, runErr := RunPipeline(context.Background(), p, PipelineRunOptions{
		RepoPath: dir,
		Stdout:   &stdout,
		Stderr:   &stderr,
	})
	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	if res.PassFail != "pass" {
		t.Errorf("PassFail = %q, want pass", res.PassFail)
	}
}

// TestRun_RequireDirtyTreeBannerOnStderr is the regression for #422:
// the CLI used to swallow the require: violation message in text mode
// (exit 1 with no output). It must now print the structured refusal
// banner to stderr — headline + porcelain stanza + source declaration
// pointer + remediation hint — before returning the run error.
func TestRun_RequireDirtyTreeBannerOnStderr(t *testing.T) {
	dir := initRepo(t, "main")
	// Make the tree dirty so clean_tree refuses.
	if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write dirty: %v", err)
	}
	taskDir := filepath.Join(dir, ".sklp", "tasks")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatalf("mkdir tasks: %v", err)
	}
	taskPath := filepath.Join(taskDir, "test.yaml")
	yaml := `version: 1
kind: pipeline
name: test
require:
  clean_tree: true
steps:
  - name: hello
    run: "true"
`
	if err := os.WriteFile(taskPath, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write task: %v", err)
	}

	var stdout, stderr bytes.Buffer
	_, err := Run(context.Background(), RunConfig{
		TaskPath: taskPath,
		RepoPath: dir,
		NoPull:   true,
		Stdout:   &stdout,
		Stderr:   &stderr,
	})
	if err == nil {
		t.Fatal("expected error from require: violation, got nil")
	}
	if !errors.Is(err, ErrRequireViolation) {
		t.Fatalf("expected ErrRequireViolation, got: %v", err)
	}
	got := stderr.String()
	for _, want := range []string{
		"refusing to run: working tree dirty",
		"untracked",
		"?? dirty.txt",
		"require.clean_tree: true",
		".sklp/tasks/test.yaml",
		"Commit or stash, then re-run",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stderr missing %q\nfull stderr:\n%s", want, got)
		}
	}
	// Text mode must NOT emit JSON sentinel markers on stderr.
	if strings.Contains(got, ReportSentinelBegin) {
		t.Errorf("text mode should not emit JSON report sentinel, got:\n%s", got)
	}
}

// TestRun_RequireBranchBannerOnStderr mirrors the previous test for the
// other supported rule: a branch mismatch must yield a banner that
// names the current and required branches and points back at the
// declaration site.
func TestRun_RequireBranchBannerOnStderr(t *testing.T) {
	dir := initRepo(t, "feat/foo")
	taskDir := filepath.Join(dir, ".sklp", "tasks")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatalf("mkdir tasks: %v", err)
	}
	taskPath := filepath.Join(taskDir, "test.yaml")
	yaml := `version: 1
kind: pipeline
name: test
require:
  branches: [main]
steps:
  - name: hello
    run: "true"
`
	if err := os.WriteFile(taskPath, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write task: %v", err)
	}
	// Commit the task file so the tree is clean and only the branch
	// check is exercised.
	cmd := exec.Command("git", "add", "-A")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "commit", "-q", "-m", "task")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	var stdout, stderr bytes.Buffer
	_, err := Run(context.Background(), RunConfig{
		TaskPath: taskPath,
		RepoPath: dir,
		NoPull:   true,
		Stdout:   &stdout,
		Stderr:   &stderr,
	})
	if err == nil {
		t.Fatal("expected error from require: violation, got nil")
	}
	if !errors.Is(err, ErrRequireViolation) {
		t.Fatalf("expected ErrRequireViolation, got: %v", err)
	}
	got := stderr.String()
	for _, want := range []string{
		"refusing to run: branch 'feat/foo' not in require.branches [main]",
		"require.branches: [main]",
		".sklp/tasks/test.yaml",
		"Switch to one of the allowed branches, then re-run",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stderr missing %q\nfull stderr:\n%s", want, got)
		}
	}
}

// TestRun_RequireDirtyTreeJSONMode locks the -output json shape: the
// CLI must emit a single JSON object on stdout with kind, error,
// pass_fail and a details map carrying the structured rule context.
// The human banner must NOT appear in this mode.
func TestRun_RequireDirtyTreeJSONMode(t *testing.T) {
	dir := initRepo(t, "main")
	if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write dirty: %v", err)
	}
	taskDir := filepath.Join(dir, ".sklp", "tasks")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatalf("mkdir tasks: %v", err)
	}
	taskPath := filepath.Join(taskDir, "test.yaml")
	yaml := `version: 1
kind: pipeline
name: test
require:
  clean_tree: true
steps:
  - name: hello
    run: "true"
`
	if err := os.WriteFile(taskPath, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write task: %v", err)
	}

	var stdout, stderr bytes.Buffer
	_, err := Run(context.Background(), RunConfig{
		TaskPath:   taskPath,
		RepoPath:   dir,
		NoPull:     true,
		JSONOutput: true,
		Stdout:     &stdout,
		Stderr:     &stderr,
	})
	if err == nil {
		t.Fatal("expected error from require: violation, got nil")
	}
	if !errors.Is(err, ErrRequireViolation) {
		t.Fatalf("expected ErrRequireViolation, got: %v", err)
	}
	out := stdout.String()
	for _, want := range []string{
		`"kind":"require_violation"`,
		`"pass_fail":"fail"`,
		`"rule":"clean_tree"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q\nfull stdout:\n%s", want, out)
		}
	}
	// JSON mode must not print the human "refusing to run" banner on
	// stderr (the JSON object on stdout is the report).
	if strings.Contains(stderr.String(), "Commit or stash, then re-run") {
		t.Errorf("json mode leaked human banner to stderr:\n%s", stderr.String())
	}
}

// TestRunPipeline_NoRequireBlockUnchanged guards the "today's behaviour
// exactly" acceptance criterion: a pipeline without require: runs in
// any directory (even one that isn't a git repo).
func TestRunPipeline_NoRequireBlockUnchanged(t *testing.T) {
	dir := t.TempDir() // NOT a git repo
	yaml := `
version: 1
kind: pipeline
steps:
  - name: noop
    run: "true"
`
	path := filepath.Join(dir, "ci.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	p, err := LoadPipeline(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var stdout, stderr bytes.Buffer
	res, runErr := RunPipeline(context.Background(), p, PipelineRunOptions{
		RepoPath: dir,
		Stdout:   &stdout,
		Stderr:   &stderr,
	})
	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	if res.PassFail != "pass" {
		t.Errorf("PassFail = %q, want pass", res.PassFail)
	}
}
