package pipeline

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A pre-loop bail-out (e.g. missing required secret) must print a clear
// failure line to stderr in text mode. Without this, `git push` hooks
// that capture stderr see only "sklp pipeline · N step(s)" + exit 1 and
// can't tell the operator what broke.
func TestRunPipelineFromBytes_PreLoopBailOutPrintsFailure(t *testing.T) {
	// A minimal pipeline that requires a secret we know isn't set. The
	// run never reaches CompilePipeline because resolveSecrets fails
	// first — exactly the path the issue #350 / hook-CI users hit.
	const yaml = `version: 1
kind: pipeline
name: test
secrets:
  - SKLP_TEST_DEFINITELY_NOT_SET
steps:
  - name: noop
    run: "true"
`

	repo := t.TempDir()
	taskPath := filepath.Join(repo, "ci.yaml")
	if err := os.WriteFile(taskPath, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write task: %v", err)
	}

	os.Unsetenv("SKLP_TEST_DEFINITELY_NOT_SET")

	var stdout, stderr bytes.Buffer
	cfg := RunConfig{
		TaskPath:   taskPath,
		RepoPath:   repo,
		Stdout:     &stdout,
		Stderr:     &stderr,
		JSONOutput: false,
	}
	_, err := runPipelineFromBytes(context.Background(), cfg, taskPath, repo, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error from missing secret bail-out")
	}

	combined := stdout.String() + stderr.String()
	if !strings.Contains(combined, "CI FAILED") {
		t.Fatalf("expected 'CI FAILED' banner in output, got:\nstdout=%q\nstderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(combined, "SKLP_TEST_DEFINITELY_NOT_SET") {
		t.Fatalf("expected the missing secret name in output, got:\nstdout=%q\nstderr=%q", stdout.String(), stderr.String())
	}
}
