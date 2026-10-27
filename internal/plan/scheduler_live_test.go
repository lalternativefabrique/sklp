package plan

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"
)

// TestScheduler_LiveReporter_ConcurrentStarts asserts that with
// LiveReporter=true and Jobs>1 the scheduler fires OnStepStart for
// both steps before either completes -- the pre-live engine serialised
// starts behind the flush mutex so a TUI could never see two spinners
// active at the same time. This is the issue-399 acceptance test.
func TestScheduler_LiveReporter_ConcurrentStarts(t *testing.T) {
	pl := &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindCI,
		CI: &CIPlan{
			Steps: []CIStep{
				{Name: "a", Run: "sleep 1", Status: StatusIncluded},
				{Name: "b", Run: "sleep 1", Status: StatusIncluded},
			},
		},
	}

	var mu sync.Mutex
	var startOrder []string
	var endOrder []string
	bothStarted := make(chan struct{})

	cfg := CIExecConfig{
		Plan: pl, Repo: t.TempDir(),
		Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
		Jobs:         2,
		LiveReporter: true,
		// MakeStepWriters is nil: scheduler still buffers per step,
		// but with LiveReporter it should fire OnStepStart immediately.
		OnStepStart: func(ev CIStepEvent) {
			mu.Lock()
			startOrder = append(startOrder, ev.Name)
			n := len(startOrder)
			mu.Unlock()
			if n == 2 {
				select {
				case <-bothStarted:
				default:
					close(bothStarted)
				}
			}
		},
		OnStepEnd: func(ev CIStepEvent) {
			mu.Lock()
			endOrder = append(endOrder, ev.Name)
			mu.Unlock()
		},
	}

	go func() {
		_, _ = ExecuteCI(context.Background(), cfg)
	}()

	select {
	case <-bothStarted:
		// pass
	case <-time.After(800 * time.Millisecond):
		mu.Lock()
		got := append([]string(nil), startOrder...)
		mu.Unlock()
		t.Fatalf("expected both StepStarted events within 800ms; got %v", got)
	}
}

// TestScheduler_DefaultSerialisesStarts is the inverse: without
// LiveReporter, the scheduler must keep its pre-2.5 contract of
// "start->flush->end atomic per step", so the second start can never
// fire while the first step is still running.
func TestScheduler_DefaultSerialisesStarts(t *testing.T) {
	pl := &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindCI,
		CI: &CIPlan{
			Steps: []CIStep{
				{Name: "a", Run: "sleep 1", Status: StatusIncluded},
				{Name: "b", Run: "sleep 1", Status: StatusIncluded},
			},
		},
	}

	var mu sync.Mutex
	starts, ends := 0, 0
	overlap := false

	cfg := CIExecConfig{
		Plan: pl, Repo: t.TempDir(),
		Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
		Jobs: 2,
		OnStepStart: func(ev CIStepEvent) {
			mu.Lock()
			starts++
			if starts-ends > 1 {
				overlap = true
			}
			mu.Unlock()
		},
		OnStepEnd: func(ev CIStepEvent) {
			mu.Lock()
			ends++
			mu.Unlock()
		},
	}
	if _, err := ExecuteCI(context.Background(), cfg); err != nil {
		t.Fatalf("execute: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if overlap {
		t.Errorf("default scheduler should serialise OnStepStart/End under flushMu, but observed overlap")
	}
	if starts != 2 || ends != 2 {
		t.Errorf("expected 2 starts + 2 ends, got starts=%d ends=%d", starts, ends)
	}
}
