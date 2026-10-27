package reporter

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// PlainReporter emits one line per event with no ANSI codes, no spinner,
// and no in-place updates. It is the safe default for non-TTY sinks
// (pipes, CI logs, file captures).
//
// The format is intentionally grep-friendly: each step transition is a
// single line beginning with the step name, so a reader can `grep ^lint`
// to follow one step across a noisy run.
type PlainReporter struct {
	W io.Writer
}

func (r *PlainReporter) Header(line string) {
	if line == "" {
		return
	}
	fmt.Fprintln(r.W, line)
}

func (r *PlainReporter) StepStarted(name string) {
	fmt.Fprintf(r.W, "%s: started\n", name)
}

func (r *PlainReporter) StepProgress(_, _ string) {
	// Progress hints are a TTY-only nicety — in non-TTY they would just
	// add noise (the step's own stdout already shows progress).
}

func (r *PlainReporter) StepFinished(name string, status Status, dur time.Duration, summary string) {
	label := string(status)
	if summary != "" {
		fmt.Fprintf(r.W, "%s: %s (%s) — %s\n", name, label, FormatDuration(dur), summary)
		return
	}
	fmt.Fprintf(r.W, "%s: %s (%s)\n", name, label, FormatDuration(dur))
}

func (r *PlainReporter) SubAction(desc string) {
	fmt.Fprintf(r.W, "  · %s\n", desc)
}

func (r *PlainReporter) Failures(title string, items []Failure) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintln(r.W)
	fmt.Fprintln(r.W, title)
	for _, f := range items {
		head := f.Title
		if f.Scope != "" {
			head = f.Scope + " > " + head
		}
		fmt.Fprintf(r.W, "  - %s\n", head)
		if f.Message != "" {
			for _, ln := range strings.Split(strings.TrimRight(f.Message, "\n"), "\n") {
				fmt.Fprintf(r.W, "      %s\n", ln)
			}
		}
		if f.Location != "" {
			fmt.Fprintf(r.W, "      %s\n", f.Location)
		}
	}
}

func (r *PlainReporter) Finalize(title string, status Status, total time.Duration, fields []FinalField, hints []FinalField) {
	verdict := "PASSED"
	if status == StatusFailed {
		verdict = "FAILED"
	}
	fmt.Fprintln(r.W)
	fmt.Fprintf(r.W, "%s %s (%s)\n", title, verdict, FormatDuration(total))
	for _, f := range fields {
		fmt.Fprintf(r.W, "  %s: %s\n", f.Label, f.Value)
	}
	for _, h := range hints {
		fmt.Fprintf(r.W, "  %s: %s\n", h.Label, h.Value)
	}
}
