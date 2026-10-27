package pipeline

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// setupScopeRepo bootstraps a git repo with one initial commit and a
// second commit that touches only the files listed in changed. The
// returned dir is the repo root, suitable as PipelineRunOptions.RepoPath.
// Skips when git is missing — same convention as TestChangedFiles_*.
func setupScopeRepo(t *testing.T, changed []string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(cmd.Env, "HOME="+dir,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "--initial-branch=main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	run("commit", "--allow-empty", "-m", "init")

	for _, rel := range changed {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("content\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", "-A")
	run("commit", "-m", "feat: changes")
	return dir
}

func TestRunPipeline_ScopeSkipsUnchangedSteps(t *testing.T) {
	// Repo state: only apps/core/handler.go changed since HEAD~1.
	// The backend step must run; the frontend step must be skipped.
	repo := setupScopeRepo(t, []string{"apps/core/handler.go"})

	yaml := `
version: 1
kind: pipeline
scopes:
  backend:  ["apps/core/**"]
  frontend: ["apps/web/**"]
steps:
  - name: always
    run: echo always-runs
  - name: test-backend
    scope: backend
    run: echo backend-ran
  - name: test-frontend
    scope: frontend
    run: echo frontend-ran
`
	path := filepath.Join(repo, "ci.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPipeline(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	var stdout, stderr bytes.Buffer
	res, err := RunPipeline(context.Background(), p, PipelineRunOptions{
		RepoPath: repo,
		BaseRef:  "HEAD~1",
		Stdout:   &stdout,
		Stderr:   &stderr,
	})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, stderr.String())
	}

	got := map[string]string{}
	for _, s := range res.Steps {
		got[s.Name] = s.Status
	}
	if got["always"] != "passed" {
		t.Errorf("always: %q, want passed", got["always"])
	}
	if got["test-backend"] != "passed" {
		t.Errorf("test-backend: %q, want passed (backend changed)", got["test-backend"])
	}
	if got["test-frontend"] != "skipped" {
		t.Errorf("test-frontend: %q, want skipped (no frontend changes)", got["test-frontend"])
	}
	if strings.Contains(stdout.String(), "frontend-ran") {
		t.Errorf("frontend step actually ran:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "backend-ran") {
		t.Errorf("backend step did not run:\n%s", stdout.String())
	}
}

func TestRunPipeline_ScopeListOR(t *testing.T) {
	// Only frontend changed; a step scoped to [backend, frontend]
	// must run via the OR semantics.
	repo := setupScopeRepo(t, []string{"apps/web/main.tsx"})

	yaml := `
version: 1
kind: pipeline
scopes:
  backend:  ["apps/core/**"]
  frontend: ["apps/web/**"]
steps:
  - name: integration
    scope: [backend, frontend]
    run: echo integration-ran
`
	path := filepath.Join(repo, "ci.yaml")
	_ = os.WriteFile(path, []byte(yaml), 0o644)
	p, err := LoadPipeline(path)
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	res, err := RunPipeline(context.Background(), p, PipelineRunOptions{
		RepoPath: repo, BaseRef: "HEAD~1", Stdout: &stdout, Stderr: &stdout,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Steps[0].Status != "passed" {
		t.Errorf("integration: %q, want passed", res.Steps[0].Status)
	}
}

func TestRunPipeline_ScopeUnknownNameFails(t *testing.T) {
	// A step references a scope that isn't declared → parse-time
	// success, but the executor returns a clear error and the step is
	// marked failed.
	repo := setupScopeRepo(t, []string{"x"})
	yaml := `
version: 1
kind: pipeline
scopes:
  backend: ["apps/core/**"]
steps:
  - name: oops
    scope: typo
    run: echo nope
`
	path := filepath.Join(repo, "ci.yaml")
	_ = os.WriteFile(path, []byte(yaml), 0o644)
	p, _ := LoadPipeline(path)
	res, err := RunPipeline(context.Background(), p, PipelineRunOptions{
		RepoPath: repo, BaseRef: "HEAD~1", Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
	})
	if err == nil {
		t.Fatal("expected error for unknown scope")
	}
	if !strings.Contains(err.Error(), "typo") {
		t.Errorf("error should name the missing scope: %v", err)
	}
	if res.Steps[0].Status != "failed" {
		t.Errorf("step status: %q, want failed", res.Steps[0].Status)
	}
}
