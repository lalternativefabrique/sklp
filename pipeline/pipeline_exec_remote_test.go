package pipeline

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/plan"
)

// fakeRemote mirrors the in-package fake from internal/plan, scoped to
// runner-level tests. We deliberately re-implement instead of exporting
// the plan-test fake (test files are not packaged for downstream
// consumers).
type runnerFakeRemote struct {
	mu       sync.Mutex
	results  map[string]*plan.RemoteStepResult
	cas      map[string][]byte
	getCalls int
	putCalls int
}

func newRunnerFakeRemote() *runnerFakeRemote {
	return &runnerFakeRemote{
		results: map[string]*plan.RemoteStepResult{},
		cas:     map[string][]byte{},
	}
}

func (f *runnerFakeRemote) GetResult(_ context.Context, k string) (*plan.RemoteStepResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	r, ok := f.results[k]
	if !ok {
		return nil, plan.ErrRemoteCacheMiss
	}
	cp := *r
	return &cp, nil
}

func (f *runnerFakeRemote) PutResult(_ context.Context, k string, r *plan.RemoteStepResult) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putCalls++
	cp := *r
	f.results[k] = &cp
	return nil
}

func (f *runnerFakeRemote) GetCAS(_ context.Context, sha string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.cas[sha]
	if !ok {
		return nil, plan.ErrRemoteCacheMiss
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (f *runnerFakeRemote) PutCAS(_ context.Context, sha string, r io.Reader, _ int64) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cas[sha] = b
	return nil
}

func (f *runnerFakeRemote) HasCAS(_ context.Context, sha string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.cas[sha]
	return ok, nil
}

func writePipelineYAML(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "ci.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return path
}

const remoteTestPipeline = `
version: 1
kind: pipeline
steps:
  - name: hello
    run: echo hello-remote
`

func TestRunPipeline_NoRemoteCacheSkipsBothLookupAndPush(t *testing.T) {
	dir := t.TempDir()
	path := writePipelineYAML(t, dir, remoteTestPipeline)
	p, err := LoadPipeline(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	remote := newRunnerFakeRemote()
	var stdout, stderr bytes.Buffer
	res, err := RunPipeline(context.Background(), p, PipelineRunOptions{
		RepoPath:      dir,
		Stdout:        &stdout,
		Stderr:        &stderr,
		Remote:        remote,
		NoRemoteCache: true,
	})
	if err != nil {
		t.Fatalf("RunPipeline: %v", err)
	}
	if res.PassFail != "pass" {
		t.Fatalf("pass_fail = %q", res.PassFail)
	}
	if remote.getCalls != 0 || remote.putCalls != 0 {
		t.Fatalf("expected zero remote traffic with --no-remote-cache, got get=%d put=%d", remote.getCalls, remote.putCalls)
	}
}

func TestRunPipeline_RemoteEnabledPushesAfterRun(t *testing.T) {
	dir := t.TempDir()
	path := writePipelineYAML(t, dir, remoteTestPipeline)
	p, err := LoadPipeline(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	remote := newRunnerFakeRemote()
	var stdout, stderr bytes.Buffer
	_, err = RunPipeline(context.Background(), p, PipelineRunOptions{
		RepoPath: dir,
		Stdout:   &stdout,
		Stderr:   &stderr,
		Remote:   remote,
	})
	if err != nil {
		t.Fatalf("RunPipeline: %v", err)
	}
	if remote.putCalls == 0 {
		t.Fatalf("expected at least one remote PutResult after a fresh run, got none")
	}
}
