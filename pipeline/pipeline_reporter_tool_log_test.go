package pipeline

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/pipeline/reporter"
)

// stubReporter is a no-op reporter that records SubAction calls only.
// We don't care about the rest for this test surface.
type stubReporter struct{ subs []string }

func (s *stubReporter) Header(string)                                               {}
func (s *stubReporter) StepStarted(string)                                          {}
func (s *stubReporter) StepProgress(string, string)                                 {}
func (s *stubReporter) StepFinished(string, reporter.Status, time.Duration, string) {}
func (s *stubReporter) SubAction(desc string)                                       { s.subs = append(s.subs, desc) }
func (s *stubReporter) Failures(string, []reporter.Failure)                         {}
func (s *stubReporter) Finalize(string, reporter.Status, time.Duration, []reporter.FinalField, []reporter.FinalField) {
}

// TestToolLogWriter_PreStepInstallsHitLiveSink covers the dash-blank-
// screen bug: when the tool resolver runs BEFORE the first StepStarted,
// `[sklp] tools: installing …` lines used to land only in the per-run
// log file (invisible to the K8s spawner's LiveOut tee), so the dash
// showed `sklp pipeline · 2 step(s)` then 10–30s of silence.
//
// Now they mirror to liveW (typically the runner's stderr → Pod stream
// → LiveOut) so the operator sees progress.
func TestToolLogWriter_PreStepInstallsHitLiveSink(t *testing.T) {
	var (
		logBuf, liveBuf bytes.Buffer
		rep             stubReporter
		stepMu          sync.Mutex
		active          bool // = false: no step started yet
	)
	w := newToolLogWriter(&rep, &logBuf, &liveBuf, &stepMu, &active)

	// Resolver writes during pre-step toolchain install.
	_, _ = w.Write([]byte("[sklp] tools: installing go@1.25.0 (native)\n"))
	_, _ = w.Write([]byte("[sklp] tools: go -> /cache/.../bin/go\n")) // noise
	_, _ = w.Write([]byte("[sklp] auto-install: go mod download\n"))

	logged := logBuf.String()
	for _, want := range []string{
		"installing go@1.25.0",
		"go -> /cache/",
		"auto-install: go mod download",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("log file missing %q\nfull: %s", want, logged)
		}
	}

	live := liveBuf.String()
	if !strings.Contains(live, "installing go@1.25.0") {
		t.Errorf("live sink missing the install line\nfull: %s", live)
	}
	if !strings.Contains(live, "auto-install: go mod download") {
		t.Errorf("live sink missing the auto-install line\nfull: %s", live)
	}
	// Pre-step: full pass-through. The previous noise filter was
	// dropping `go: downloading …` style lines from `go mod download
	// -x` and leaving the live sink silent for minutes. Every line the
	// resolver / auto-install child emits must reach the operator's
	// live tail during the pre-step phase — even the "noisy" ones.
	if !strings.Contains(live, "/cache/") {
		t.Errorf("live sink should pass through noisy lines pre-step too; got %q", live)
	}

	if len(rep.subs) != 0 {
		t.Errorf("SubAction called pre-step; got %v", rep.subs)
	}
}

// TestToolLogWriter_PreStepPassesAutoInstallChildOutput guards the
// regression that motivated the pre-step pass-through: an auto-install
// command (e.g. `go mod download -x`) emits its own progress lines on
// the child's stdout/stderr, and those lines must hit liveW so the
// operator sees the download moving rather than a frozen header.
func TestToolLogWriter_PreStepPassesAutoInstallChildOutput(t *testing.T) {
	var (
		logBuf, liveBuf bytes.Buffer
		rep             stubReporter
		stepMu          sync.Mutex
		active          bool
	)
	w := newToolLogWriter(&rep, &logBuf, &liveBuf, &stepMu, &active)

	_, _ = w.Write([]byte("[sklp] auto-install: go mod download -x\n"))
	_, _ = w.Write([]byte("# get https://proxy.golang.org/github.com/foo/@v/list\n"))
	_, _ = w.Write([]byte("go: downloading github.com/foo/bar v1.2.3\n"))

	live := liveBuf.String()
	for _, want := range []string{
		"auto-install: go mod download -x",
		"# get https://proxy.golang.org",
		"go: downloading github.com/foo/bar",
	} {
		if !strings.Contains(live, want) {
			t.Errorf("live sink missing %q\nfull: %s", want, live)
		}
	}
}

// TestToolLogWriter_ActiveStepRoutesToSubAction guards the original
// in-step behaviour: when a step is on screen, installs go through
// rep.SubAction (the live sink stays quiet to avoid double-printing).
func TestToolLogWriter_ActiveStepRoutesToSubAction(t *testing.T) {
	var (
		logBuf, liveBuf bytes.Buffer
		rep             stubReporter
		stepMu          sync.Mutex
		active          = true
	)
	w := newToolLogWriter(&rep, &logBuf, &liveBuf, &stepMu, &active)

	_, _ = w.Write([]byte("[sklp] tools: installing node@22.19.0 (native)\n"))
	_, _ = w.Write([]byte("[sklp] auto-install: pnpm install\n"))

	if got := len(rep.subs); got != 2 {
		t.Fatalf("SubAction expected 2 calls, got %d (%v)", got, rep.subs)
	}
	if rep.subs[0] != "installing node 22.19.0" {
		t.Errorf("SubAction[0] = %q", rep.subs[0])
	}
	if rep.subs[1] != "auto-install: pnpm install" {
		t.Errorf("SubAction[1] = %q", rep.subs[1])
	}
	if liveBuf.Len() != 0 {
		t.Errorf("live sink should be quiet while a step is active, got %q", liveBuf.String())
	}
}
