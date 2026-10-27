package plan

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestScheduler_ParallelismObservable asserts two independent steps with
// no dependency edge between them run concurrently — total wall-clock is
// bounded by the longest single step plus headroom, not their sum.
func TestScheduler_ParallelismObservable(t *testing.T) {
	pl := &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindCI,
		CI: &CIPlan{
			Steps: []CIStep{
				{Name: "a", Run: "sleep 1 && echo a", Status: StatusIncluded},
				{Name: "b", Run: "sleep 1 && echo b", Status: StatusIncluded},
			},
		},
	}
	var out bytes.Buffer
	t0 := time.Now()
	rep, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan: pl, Repo: t.TempDir(), Stdout: &out, Stderr: &out, Jobs: 2,
	})
	dur := time.Since(t0)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if rep.PassFail != "pass" {
		t.Fatalf("expected pass, got %q", rep.PassFail)
	}
	if dur >= 1900*time.Millisecond {
		t.Errorf("expected parallel execution (~1s), took %v", dur)
	}
}

// TestScheduler_SequentialJobs1 asserts Jobs=1 reproduces sequential
// semantics: two independent steps run one after the other.
func TestScheduler_SequentialJobs1(t *testing.T) {
	pl := &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindCI,
		CI: &CIPlan{
			Steps: []CIStep{
				{Name: "a", Run: "sleep 1 && echo a", Status: StatusIncluded},
				{Name: "b", Run: "sleep 1 && echo b", Status: StatusIncluded},
			},
		},
	}
	var out bytes.Buffer
	t0 := time.Now()
	_, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan: pl, Repo: t.TempDir(), Stdout: &out, Stderr: &out, Jobs: 1,
	})
	dur := time.Since(t0)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if dur < 1900*time.Millisecond {
		t.Errorf("Jobs=1 should be sequential (~2s), took %v", dur)
	}
}

// TestScheduler_DeterministicReport runs the same pipeline twice and
// asserts the JSON-encoded reports are byte-identical (after stripping
// timing fields).
func TestScheduler_DeterministicReport(t *testing.T) {
	mkPlan := func() *Plan {
		return &Plan{
			SchemaVersion: SchemaVersion,
			Kind:          KindCI,
			CI: &CIPlan{
				Steps: []CIStep{
					{Name: "a", Run: "echo a", Status: StatusIncluded},
					{Name: "b", Run: "echo b", Status: StatusIncluded},
					{Name: "c", Run: "echo c", Status: StatusIncluded, DependsOn: []string{"a", "b"}},
				},
			},
		}
	}
	var out1, out2 bytes.Buffer
	r1, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan: mkPlan(), Repo: t.TempDir(), Stdout: &out1, Stderr: &out1, Jobs: 4,
	})
	if err != nil {
		t.Fatalf("run1: %v", err)
	}
	r2, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan: mkPlan(), Repo: t.TempDir(), Stdout: &out2, Stderr: &out2, Jobs: 4,
	})
	if err != nil {
		t.Fatalf("run2: %v", err)
	}
	b1, _ := json.Marshal(r1)
	b2, _ := json.Marshal(r2)
	durRe := regexp.MustCompile(`"duration_ms":\d+`)
	rtRe := regexp.MustCompile(`"runtime_ms":\d+`)
	s1 := rtRe.ReplaceAllString(durRe.ReplaceAllString(string(b1), `"duration_ms":0`), `"runtime_ms":0`)
	s2 := rtRe.ReplaceAllString(durRe.ReplaceAllString(string(b2), `"duration_ms":0`), `"runtime_ms":0`)
	if s1 != s2 {
		t.Errorf("reports differ:\n%s\n---\n%s", s1, s2)
	}
}

// TestScheduler_NoLogCorruption asserts a step's lines are flushed
// atomically — no interleaving with another parallel step.
func TestScheduler_NoLogCorruption(t *testing.T) {
	pl := &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindCI,
		CI: &CIPlan{
			Steps: []CIStep{
				{Name: "a", Run: `for i in 1 2 3 4 5; do echo "A-$i"; sleep 0.05; done`, Status: StatusIncluded},
				{Name: "b", Run: `for i in 1 2 3 4 5; do echo "B-$i"; sleep 0.05; done`, Status: StatusIncluded},
			},
		},
	}
	out := &syncBuffer{}
	_, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan: pl, Repo: t.TempDir(), Stdout: out, Stderr: out, Jobs: 2,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	// Output must contain one contiguous A-block and one contiguous B-block.
	text := out.String()
	aBlock := "A-1\nA-2\nA-3\nA-4\nA-5\n"
	bBlock := "B-1\nB-2\nB-3\nB-4\nB-5\n"
	if !strings.Contains(text, aBlock) {
		t.Errorf("A lines were interleaved or missing: %q", text)
	}
	if !strings.Contains(text, bBlock) {
		t.Errorf("B lines were interleaved or missing: %q", text)
	}
}

// TestScheduler_FailurePropagation: A fails -> B (depends on A) and C
// (depends on B) become skipped. D (no dep on A) keeps running.
func TestScheduler_FailurePropagation(t *testing.T) {
	pl := &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindCI,
		CI: &CIPlan{
			Steps: []CIStep{
				{Name: "a", Run: "exit 1", Status: StatusIncluded},
				{Name: "b", Run: "echo b", Status: StatusIncluded, DependsOn: []string{"a"}},
				{Name: "c", Run: "echo c", Status: StatusIncluded, DependsOn: []string{"b"}},
				{Name: "d", Run: "echo d", Status: StatusIncluded},
			},
		},
	}
	var out bytes.Buffer
	rep, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan: pl, Repo: t.TempDir(), Stdout: &out, Stderr: &out, Jobs: 4,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	byName := map[string]CIStepResult{}
	for _, s := range rep.Steps {
		byName[s.Name] = s
	}
	if byName["a"].Status != "failed" {
		t.Errorf("a: want failed, got %q", byName["a"].Status)
	}
	if byName["b"].Status != "skipped" {
		t.Errorf("b: want skipped, got %q", byName["b"].Status)
	}
	if byName["c"].Status != "skipped" {
		t.Errorf("c: want skipped, got %q", byName["c"].Status)
	}
	if byName["d"].Status != "passed" {
		t.Errorf("d (sibling): want passed, got %q", byName["d"].Status)
	}
}

// TestScheduler_FailFastCancelsInFlight: with FailFast=true, a long
// sibling step gets cancelled when another step fails.
func TestScheduler_FailFastCancelsInFlight(t *testing.T) {
	pl := &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindCI,
		CI: &CIPlan{
			Steps: []CIStep{
				{Name: "fail", Run: "sleep 0.1 && exit 1", Status: StatusIncluded},
				{Name: "long", Run: "sleep 10 && echo done", Status: StatusIncluded},
			},
		},
	}
	var out bytes.Buffer
	t0 := time.Now()
	rep, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan: pl, Repo: t.TempDir(), Stdout: &out, Stderr: &out,
		Jobs: 2, FailFast: true,
	})
	dur := time.Since(t0)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if dur > 5*time.Second {
		t.Errorf("FailFast didn't cancel: took %v", dur)
	}
	byName := map[string]CIStepResult{}
	for _, s := range rep.Steps {
		byName[s.Name] = s
	}
	if byName["fail"].Status != "failed" {
		t.Errorf("fail: want failed, got %q", byName["fail"].Status)
	}
	// `long` was cancelled mid-flight: it surfaces as failed (non-zero
	// exit from the killed process). The point is the run did not hang.
	if byName["long"].Status == "passed" {
		t.Errorf("long should not have completed: %+v", byName["long"])
	}
}

// TestScheduler_TopoOrderInResults asserts the Steps slice in the
// report follows the declared topo order regardless of completion order.
func TestScheduler_TopoOrderInResults(t *testing.T) {
	pl := &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindCI,
		CI: &CIPlan{
			Steps: []CIStep{
				{Name: "slow", Run: "sleep 0.3 && echo s", Status: StatusIncluded},
				{Name: "fast", Run: "echo f", Status: StatusIncluded},
			},
		},
	}
	var out bytes.Buffer
	rep, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan: pl, Repo: t.TempDir(), Stdout: &out, Stderr: &out, Jobs: 2,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(rep.Steps) != 2 || rep.Steps[0].Name != "slow" || rep.Steps[1].Name != "fast" {
		t.Errorf("topo order not preserved: %+v", rep.Steps)
	}
}

// syncBuffer is a goroutine-safe bytes.Buffer for tests under -race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
