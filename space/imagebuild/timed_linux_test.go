//go:build linux

package imagebuild

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestTimed_ReportsSlowPhase proves a phase at or above timedFloor gets a
// duration line, so a build log attributes its wall-clock instead of only
// listing what ran.
func TestTimed_ReportsSlowPhase(t *testing.T) {
	var buf bytes.Buffer
	opts := Options{Progress: &buf}

	if err := opts.timed("slow phase", func() error {
		time.Sleep(timedFloor + 50*time.Millisecond)
		return nil
	}); err != nil {
		t.Fatalf("timed returned %v, want nil", err)
	}

	got := buf.String()
	if !strings.Contains(got, "slow phase took ") {
		t.Errorf("progress = %q, want it to report the phase duration", got)
	}
}

// TestTimed_SilentBelowFloor proves sub-floor phases stay silent: a build with
// many trivial instructions must not drown its log in "0.0s" noise.
func TestTimed_SilentBelowFloor(t *testing.T) {
	var buf bytes.Buffer
	opts := Options{Progress: &buf}

	if err := opts.timed("fast phase", func() error { return nil }); err != nil {
		t.Fatalf("timed returned %v, want nil", err)
	}

	if got := buf.String(); got != "" {
		t.Errorf("progress = %q, want no output for a sub-floor phase", got)
	}
}

// TestTimed_PropagatesError proves timed is transparent to the caller: it
// reports timing but never swallows or rewrites the wrapped error, which the
// build loop relies on to abort the stage.
func TestTimed_PropagatesError(t *testing.T) {
	var buf bytes.Buffer
	opts := Options{Progress: &buf}
	want := errors.New("boom")

	if got := opts.timed("failing phase", func() error { return want }); !errors.Is(got, want) {
		t.Errorf("timed returned %v, want %v", got, want)
	}
}

// TestTimed_NilProgressStillRuns proves the no-progress path (Progress nil, the
// silent default) still executes the work rather than skipping it.
func TestTimed_NilProgressStillRuns(t *testing.T) {
	var opts Options // Progress nil
	ran := false

	if err := opts.timed("phase", func() error { ran = true; return nil }); err != nil {
		t.Fatalf("timed returned %v, want nil", err)
	}
	if !ran {
		t.Error("timed did not run fn when Progress was nil")
	}
}
