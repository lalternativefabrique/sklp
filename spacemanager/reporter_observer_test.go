package spacemanager

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/pipeline/reporter"
)

// spyReporter is a streaming reporter that records every StepLine it receives,
// so a test can assert the exact text the LogSink renders (one prefix, no
// doubling). It implements the streamingReporter the observer promotes to.
type spyReporter struct {
	lines []string
}

func (s *spyReporter) Header(string)                                               {}
func (s *spyReporter) StepStarted(string)                                          {}
func (s *spyReporter) StepProgress(string, string)                                 {}
func (s *spyReporter) StepFinished(string, reporter.Status, time.Duration, string) {}
func (s *spyReporter) SubAction(string)                                            {}
func (s *spyReporter) Failures(string, []reporter.Failure)                         {}
func (s *spyReporter) Finalize(string, reporter.Status, time.Duration, []reporter.FinalField, []reporter.FinalField) {
}
func (s *spyReporter) StepLine(_ string, line string, _ bool) { s.lines = append(s.lines, line) }
func (s *spyReporter) EnableStreaming()                       {}
func (s *spyReporter) DisableLinePrefix()                     {}

// TestLogSinkSinglePrefix locks the double-prefix fix at the deploy layer: a
// raw container line "api up" tagged to service "api" must render exactly
// "[api] api up" — one prefix, never "[api] [api] api up".
func TestLogSinkSinglePrefix(t *testing.T) {
	spy := &spyReporter{}
	o := newReporterObserverFrom(spy, SpaceMeta{Name: "prod"}, []Service{{Name: "api"}})
	o.LogSink()("api", "api up")

	if len(spy.lines) != 1 {
		t.Fatalf("expected exactly one streamed line, got %d: %v", len(spy.lines), spy.lines)
	}
	if got, want := spy.lines[0], "[api] api up"; got != want {
		t.Fatalf("LogSink rendered %q, want %q (single prefix)", got, want)
	}
}

func TestStateStatusMapping(t *testing.T) {
	cases := []struct {
		in   State
		want reporter.Status
	}{
		{Pending, reporter.StatusRunning},
		{Starting, reporter.StatusRunning},
		{Restarting, reporter.StatusRunning},
		{Ready, reporter.StatusPassed},
		{Failed, reporter.StatusFailed},
	}
	for _, c := range cases {
		if got := stateStatus(c.in); got != c.want {
			t.Errorf("stateStatus(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A bytes.Buffer is not an *os.File, so reporter.New returns a PlainReporter —
// the non-TTY fallback. We assert the sequential lines for a realistic event
// sequence (start → ready, plus a crash-loop service that recovers).
func TestReporterObserverPlainSequence(t *testing.T) {
	var buf bytes.Buffer
	meta := SpaceMeta{Name: "prod"}
	services := []Service{{Name: "api"}, {Name: "db"}}
	o := NewReporterObserver(&buf, meta, services)
	obs := o.Observer()

	// db comes up cleanly.
	obs(StateEvent{Service: "db", From: Pending, To: Starting, Detail: "db-0"})
	obs(StateEvent{Service: "db", From: Starting, To: Ready, Detail: "db-0: ready after 1.2s"})
	// api crashes once, then recovers.
	obs(StateEvent{Service: "api", From: Pending, To: Starting, Detail: "api-0"})
	obs(StateEvent{Service: "api", From: Starting, To: Failed, Detail: "api-0: probe timeout"})
	obs(StateEvent{Service: "api", From: Failed, To: Restarting, Detail: "api-0: restart in 1s"})
	obs(StateEvent{Service: "api", From: Restarting, To: Starting, Detail: "api-0"})
	obs(StateEvent{Service: "api", From: Starting, To: Ready, Detail: "api-0: ready after 2.0s"})

	o.Finalize(meta, services, nil)

	out := buf.String()
	// Both services were announced.
	for _, want := range []string{"api: started", "db: started"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
	// Both froze passed (Ready).
	if !strings.Contains(out, "db: passed") {
		t.Errorf("db never frozen passed:\n%s", out)
	}
	if !strings.Contains(out, "api: passed") {
		t.Errorf("api never frozen passed:\n%s", out)
	}
	// Summary reports all ready, no failure verdict.
	if !strings.Contains(out, "2 services ready") {
		t.Errorf("summary missing ready count:\n%s", out)
	}
	if strings.Contains(out, "FAILED") {
		t.Errorf("clean deploy should not print FAILED:\n%s", out)
	}
}

// A service that never reaches Ready must surface in the summary as not-ready
// and flip the final verdict to FAILED.
func TestReporterObserverNotReadySummary(t *testing.T) {
	var buf bytes.Buffer
	meta := SpaceMeta{Name: "prod"}
	services := []Service{{Name: "api"}, {Name: "broken"}}
	o := NewReporterObserver(&buf, meta, services)
	obs := o.Observer()

	obs(StateEvent{Service: "api", From: Pending, To: Starting})
	obs(StateEvent{Service: "api", From: Starting, To: Ready, Detail: "ready"})
	// broken stays Starting then crash-loops; never Ready.
	obs(StateEvent{Service: "broken", From: Pending, To: Starting})
	obs(StateEvent{Service: "broken", From: Starting, To: Failed, Detail: "exited"})

	o.Finalize(meta, services, nil)

	out := buf.String()
	if !strings.Contains(out, "FAILED") {
		t.Errorf("expected FAILED verdict:\n%s", out)
	}
	if !strings.Contains(out, "broken") {
		t.Errorf("expected broken listed not-ready:\n%s", out)
	}
	if !strings.Contains(out, "1 service ready") {
		t.Errorf("expected 1 service ready:\n%s", out)
	}
}
