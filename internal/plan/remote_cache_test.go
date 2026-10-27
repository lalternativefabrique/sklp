package plan

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRemote is a hermetic in-memory implementation of plan.RemoteCache
// for tests. Mirrors the production remotecache.Fake but lives in the
// plan package to avoid an import cycle (internal/remotecache adapts
// itself onto this interface via runner.remoteCacheAdapter).
type fakeRemote struct {
	mu      sync.Mutex
	results map[string]*RemoteStepResult
	cas     map[string][]byte

	failGetResult atomic.Bool
	failPutCAS    atomic.Bool
}

func newFakeRemote() *fakeRemote {
	return &fakeRemote{
		results: map[string]*RemoteStepResult{},
		cas:     map[string][]byte{},
	}
}

func (f *fakeRemote) GetResult(_ context.Context, stepKey string) (*RemoteStepResult, error) {
	if f.failGetResult.Load() {
		return nil, errors.New("simulated GetResult failure")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.results[stepKey]
	if !ok {
		return nil, ErrRemoteCacheMiss
	}
	cp := *r
	cp.Stdout = append([]byte(nil), r.Stdout...)
	cp.Stderr = append([]byte(nil), r.Stderr...)
	cp.Outputs = append([]RemoteCacheOutput(nil), r.Outputs...)
	return &cp, nil
}

func (f *fakeRemote) PutResult(_ context.Context, stepKey string, r *RemoteStepResult) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *r
	cp.Stdout = append([]byte(nil), r.Stdout...)
	cp.Stderr = append([]byte(nil), r.Stderr...)
	cp.Outputs = append([]RemoteCacheOutput(nil), r.Outputs...)
	f.results[stepKey] = &cp
	return nil
}

func (f *fakeRemote) GetCAS(_ context.Context, sha string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.cas[sha]
	if !ok {
		return nil, ErrRemoteCacheMiss
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (f *fakeRemote) PutCAS(_ context.Context, sha string, r io.Reader, _ int64) error {
	if f.failPutCAS.Load() {
		return errors.New("simulated PutCAS failure")
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cas[sha] = b
	return nil
}

func (f *fakeRemote) HasCAS(_ context.Context, sha string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.cas[sha]
	return ok, nil
}

func (f *fakeRemote) Len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.results)
}

// runWithRemote is a convenience wrapper around ExecuteCI that drains
// every async remote push before returning. ExecuteCI already blocks
// on the push WaitGroup via drainRemotePushes; the helper just sets a
// short timeout so a misbehaving test fails fast instead of stalling.
func runWithRemote(t *testing.T, cfg CIExecConfig) *CIReport {
	t.Helper()
	cfg.RemotePushTimeout = 5 * time.Second
	rep, err := ExecuteCI(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ExecuteCI: %v", err)
	}
	return rep
}

func newCIPlanWithOutputs(name, run, output string) *Plan {
	return &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindCI,
		CI: &CIPlan{
			Steps: []CIStep{
				{Name: name, Run: run, Status: StatusIncluded, Outputs: []string{output}},
			},
		},
	}
}

func TestRemoteCache_PushAfterSuccess(t *testing.T) {
	repo := t.TempDir()
	remote := newFakeRemote()
	pl := newCIPlanWithOutputs("build", "echo data > out.txt", "out.txt")

	var out bytes.Buffer
	_ = runWithRemote(t, CIExecConfig{
		Plan: pl, Repo: repo, Stdout: &out, Stderr: &out,
		Cache: OpenStepCache(repo), CAS: OpenCAS(repo),
		PlanKey: "k1", Remote: remote,
	})

	if remote.Len() != 1 {
		t.Fatalf("expected 1 remote result, got %d", remote.Len())
	}
}

func TestRemoteCache_CrossRunHit(t *testing.T) {
	// First run populates a fake remote, second run (fresh local) is
	// served entirely from it. This is the cross-machine acceptance
	// criterion of the epic, simulated in-process.
	remote := newFakeRemote()
	pl := newCIPlanWithOutputs("build", "echo data > out.txt", "out.txt")

	repo1 := t.TempDir()
	var out1 bytes.Buffer
	rep1 := runWithRemote(t, CIExecConfig{
		Plan: pl, Repo: repo1, Stdout: &out1, Stderr: &out1,
		Cache: OpenStepCache(repo1), CAS: OpenCAS(repo1),
		PlanKey: "k1", Remote: remote,
	})
	if rep1.Steps[0].Status != "passed" {
		t.Fatalf("first run not passed: %+v", rep1.Steps)
	}

	repo2 := t.TempDir()
	var hits int32
	events := make(chan string, 8)
	var out2 bytes.Buffer
	rep2 := runWithRemote(t, CIExecConfig{
		Plan: pl, Repo: repo2, Stdout: &out2, Stderr: &out2,
		Cache: OpenStepCache(repo2), CAS: OpenCAS(repo2),
		PlanKey: "k1", Remote: remote,
		OnRemoteEvent: func(_, ev string) {
			if ev == "hit" {
				atomic.AddInt32(&hits, 1)
			}
			select {
			case events <- ev:
			default:
			}
		},
	})
	if rep2.Steps[0].Status != "cached" {
		t.Fatalf("second run status = %q, want cached: %+v", rep2.Steps[0].Status, rep2.Steps)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("expected 1 remote hit, got %d", hits)
	}
	// Output file restored on repo2 from the remote-pulled CAS.
	if _, err := os.Stat(filepath.Join(repo2, "out.txt")); err != nil {
		t.Fatalf("restored output missing: %v", err)
	}
}

func TestRemoteCache_PushFailureDoesNotFailStep(t *testing.T) {
	repo := t.TempDir()
	remote := newFakeRemote()
	remote.failPutCAS.Store(true)
	pl := newCIPlanWithOutputs("build", "echo data > out.txt", "out.txt")

	var out bytes.Buffer
	rep := runWithRemote(t, CIExecConfig{
		Plan: pl, Repo: repo, Stdout: &out, Stderr: &out,
		Cache: OpenStepCache(repo), CAS: OpenCAS(repo),
		PlanKey: "k1", Remote: remote,
	})
	if rep.Steps[0].Status != "passed" {
		t.Fatalf("step should still pass on remote push failure: %+v", rep.Steps)
	}
	if !strings.Contains(out.String(), "remote cache") {
		t.Fatalf("expected remote-cache warning in stderr, got: %s", out.String())
	}
}

func TestRemoteCache_GetFailureFallsBackToLocalExec(t *testing.T) {
	repo := t.TempDir()
	remote := newFakeRemote()
	remote.failGetResult.Store(true)
	pl := newCIPlan("echo ok")

	var out bytes.Buffer
	rep := runWithRemote(t, CIExecConfig{
		Plan: pl, Repo: repo, Stdout: &out, Stderr: &out,
		Cache: OpenStepCache(repo), CAS: OpenCAS(repo),
		PlanKey: "k1", Remote: remote,
	})
	if rep.Steps[0].Status != "passed" {
		t.Fatalf("expected passed on remote GET failure (local exec), got: %+v", rep.Steps)
	}
	if !strings.Contains(out.String(), "remote cache") {
		t.Fatalf("expected remote-cache warning, got: %s", out.String())
	}
}

func TestRemoteCache_NoRemoteWhenNil(t *testing.T) {
	// Sanity check: ExecuteCI works fine with Remote == nil. The push
	// goroutine path must not even spawn.
	repo := t.TempDir()
	pl := newCIPlan("true")
	var out bytes.Buffer
	rep, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan: pl, Repo: repo, Stdout: &out, Stderr: &out,
		Cache: OpenStepCache(repo), PlanKey: "k1",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if rep.Steps[0].Status != "passed" {
		t.Fatalf("step: %+v", rep.Steps)
	}
}
