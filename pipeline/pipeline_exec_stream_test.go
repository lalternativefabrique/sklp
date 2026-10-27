package pipeline

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/pipeline/reporter"
)

// recordingReporter is a fake reporter implementing both
// MultiStepReporter and StepStreamer so the runner wires it via the
// live + streaming path. Each event records a timestamp so the test
// can assert that StepLine calls land BEFORE the StepFinished call
// for the same step — that's the regression contract live streaming
// is meant to enforce.
type recordingReporter struct {
	mu sync.Mutex

	lines    []recordedLine
	finished map[string]time.Time
}

type recordedLine struct {
	at   time.Time
	step string
	line string
}

func (r *recordingReporter) Header(string)               {}
func (r *recordingReporter) StepStarted(string)          {}
func (r *recordingReporter) StepProgress(string, string) {}
func (r *recordingReporter) StepFinished(name string, _ reporter.Status, _ time.Duration, _ string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished == nil {
		r.finished = map[string]time.Time{}
	}
	r.finished[name] = time.Now()
}
func (r *recordingReporter) SubAction(string)                    {}
func (r *recordingReporter) Failures(string, []reporter.Failure) {}
func (r *recordingReporter) Finalize(string, reporter.Status, time.Duration, []reporter.FinalField, []reporter.FinalField) {
}
func (r *recordingReporter) SupportsMultiStep() {}
func (r *recordingReporter) StepLine(name string, line string, _ bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, recordedLine{at: time.Now(), step: name, line: line})
}

func (r *recordingReporter) linesFor(step string) []recordedLine {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedLine, 0, len(r.lines))
	for _, l := range r.lines {
		if l.step == step {
			out = append(out, l)
		}
	}
	return out
}

// TestRunPipeline_StreamingDeliversLinesBeforeStepEnd is the closing
// acceptance test for the live-streaming feature (issue #399). A step
// that emits one line per ~50 ms must hand each line to the reporter
// while the step is still running, not in a single burst at the end.
func TestRunPipeline_StreamingDeliversLinesBeforeStepEnd(t *testing.T) {
	dir := t.TempDir()
	yaml := `
version: 1
kind: pipeline
steps:
  - name: slow
    run: |
      echo line1
      sleep 0.1
      echo line2
      sleep 0.1
      echo line3
`
	path := filepath.Join(dir, "ci.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	p, err := LoadPipeline(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	rep := &recordingReporter{}
	var stdout, stderr bytes.Buffer
	_, runErr := RunPipeline(context.Background(), p, PipelineRunOptions{
		RepoPath:         dir,
		Stdout:           &stdout,
		Stderr:           &stderr,
		reporterOverride: rep,
	})
	if runErr != nil {
		t.Fatalf("RunPipeline: %v", runErr)
	}

	got := rep.linesFor("slow")
	want := []string{"line1", "line2", "line3"}
	if len(got) < len(want) {
		t.Fatalf("got %d lines, want at least %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].line != w {
			t.Errorf("line[%d] = %q, want %q", i, got[i].line, w)
		}
	}

	rep.mu.Lock()
	finishAt, ok := rep.finished["slow"]
	rep.mu.Unlock()
	if !ok {
		t.Fatal("StepFinished never called for 'slow'")
	}
	// Acceptance criterion 1: every streamed line landed before the
	// step's StepFinished call. If the runner buffered until end the
	// timestamps would be the other way round.
	for _, l := range got {
		if !l.at.Before(finishAt) {
			t.Errorf("line %q landed at %v, after StepFinished at %v",
				l.line, l.at, finishAt)
		}
	}
}

// TestRunPipeline_QuietBypassesStreamer confirms the -quiet flag opts
// OUT of streaming: StepLine must never be called even when the
// reporter implements StepStreamer.
func TestRunPipeline_QuietBypassesStreamer(t *testing.T) {
	dir := t.TempDir()
	yaml := `
version: 1
kind: pipeline
steps:
  - name: chatty
    run: |
      echo one
      echo two
`
	path := filepath.Join(dir, "ci.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	p, err := LoadPipeline(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	rep := &recordingReporter{}
	var stdout, stderr bytes.Buffer
	_, runErr := RunPipeline(context.Background(), p, PipelineRunOptions{
		RepoPath:         dir,
		Stdout:           &stdout,
		Stderr:           &stderr,
		Quiet:            true,
		reporterOverride: rep,
	})
	if runErr != nil {
		t.Fatalf("RunPipeline: %v", runErr)
	}
	if got := rep.linesFor("chatty"); len(got) != 0 {
		t.Fatalf("Quiet=true must not deliver streamed lines, got %+v", got)
	}
}

// TestRunPipeline_StreamingDoesNotAlterLogCapture pins acceptance
// criterion 4: the per-run log file is identical regardless of the
// streaming setting. We can't byte-compare the log path because the
// timestamp differs between runs, but the line set must match.
func TestRunPipeline_StreamingDoesNotAlterLogCapture(t *testing.T) {
	yaml := `
version: 1
kind: pipeline
steps:
  - name: produce
    run: |
      echo alpha
      echo beta
      echo gamma
`
	// Isolate the central log store so the run log lands in a temp cache, not
	// the developer's real ~/.cache.
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	logs := map[string]string{}
	for _, mode := range []string{"stream", "quiet"} {
		dir := t.TempDir()
		path := filepath.Join(dir, "ci.yaml")
		if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
			t.Fatalf("write yaml: %v", err)
		}
		p, err := LoadPipeline(path)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		rep := &recordingReporter{}
		var stdout, stderr bytes.Buffer
		_, runErr := RunPipeline(context.Background(), p, PipelineRunOptions{
			RepoPath:         dir,
			Stdout:           &stdout,
			Stderr:           &stderr,
			Quiet:            mode == "quiet",
			reporterOverride: rep,
		})
		if runErr != nil {
			t.Fatalf("RunPipeline (%s): %v", mode, runErr)
		}
		logDir := CentralLogDir(dir)
		entries, err := os.ReadDir(logDir)
		if err != nil {
			t.Fatalf("read logs dir (%s): %v", mode, err)
		}
		if len(entries) == 0 {
			t.Fatalf("no log file written (%s)", mode)
		}
		b, err := os.ReadFile(filepath.Join(logDir, entries[0].Name()))
		if err != nil {
			t.Fatalf("read log (%s): %v", mode, err)
		}
		logs[mode] = stripVolatile(b)
	}
	if logs["stream"] == "" {
		t.Fatal("empty stream log")
	}
	if logs["stream"] != logs["quiet"] {
		t.Errorf("log capture diverges between stream and quiet:\nstream=%q\nquiet=%q",
			logs["stream"], logs["quiet"])
	}
}

// TestRunPipeline_StreamingParallelStepsPreservesOrder pins
// acceptance criterion 3: with -jobs > 1, lines from concurrent steps
// may interleave across steps but MUST stay in declared order within
// each step's own line set.
func TestRunPipeline_StreamingParallelStepsPreservesOrder(t *testing.T) {
	dir := t.TempDir()
	yaml := `
version: 1
kind: pipeline
steps:
  - name: alpha
    run: |
      for i in 1 2 3 4 5; do
        echo a-$i
      done
  - name: beta
    run: |
      for i in 1 2 3 4 5; do
        echo b-$i
      done
`
	path := filepath.Join(dir, "ci.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	p, err := LoadPipeline(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	rep := &recordingReporter{}
	var stdout, stderr bytes.Buffer
	_, runErr := RunPipeline(context.Background(), p, PipelineRunOptions{
		RepoPath:         dir,
		Stdout:           &stdout,
		Stderr:           &stderr,
		Jobs:             4,
		reporterOverride: rep,
	})
	if runErr != nil {
		t.Fatalf("RunPipeline: %v", runErr)
	}
	checkSeq := func(step, prefix string) {
		got := rep.linesFor(step)
		if len(got) < 5 {
			t.Fatalf("step %q produced %d lines, want >=5: %+v", step, len(got), got)
		}
		for i := 0; i < 5; i++ {
			want := prefix + "-" + itoa(i+1)
			if got[i].line != want {
				t.Errorf("step %q line[%d] = %q, want %q", step, i, got[i].line, want)
			}
		}
	}
	checkSeq("alpha", "a")
	checkSeq("beta", "b")
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

// stripVolatile removes the per-run "started" / repo path header
// lines so two runs' logs can be byte-compared. Anything else
// (sklp-emitted lines OR step output) is timestamp-free.
func stripVolatile(b []byte) string {
	out := make([]string, 0)
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(ln, "# started:") {
			continue
		}
		if strings.HasPrefix(ln, "# repo:") {
			continue
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}
