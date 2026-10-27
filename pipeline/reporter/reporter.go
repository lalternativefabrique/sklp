// Package reporter renders sklp's CLI progress and final status banners
// in a pretty, human-friendly format.
//
// Two implementations satisfy the Reporter interface:
//
//   - PrettyReporter — TTY-only, ANSI colours, UTF-8 symbols, banners.
//   - PlainReporter  — pipe / CI-friendly, no colour, one line per event.
//
// New(w, opts) picks the right one based on opts + terminal capability,
// so callers don't have to branch on isatty() / NO_COLOR themselves.
//
// The interface is intentionally small. Adding a new event type means
// touching every reporter, which is the right tradeoff for a UX layer:
// it forces both renderers to stay in sync.
package reporter

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Status is the terminal state of a step / final run.
type Status string

const (
	StatusPassed  Status = "passed"
	StatusFailed  Status = "failed"
	StatusSkipped Status = "skipped"
	StatusRunning Status = "running"
	StatusCached  Status = "cached"
)

// Failure is one entry in the final FAILURES section. Used by both `sklp
// run` (failed tests) and the diagnose / init `REASON` block.
type Failure struct {
	Scope    string // e.g. "billing", or "" for free-form
	Title    string // e.g. "TestCreateCheckout/paid_plan"
	Message  string // body lines
	Location string // file:line, optional

	// PreserveBytes asks the renderer to keep Message verbatim — no
	// re-wrapping, no re-indentation that desyncs columns. Set this when
	// Message contains ASCII art / tabular content (Trivy table, etc.)
	// where every byte position carries layout information.
	// #444 / #440: Trivy ASCII tables rendered as a column of empty `|`
	// characters before this flag existed.
	PreserveBytes bool
}

// FinalField is one line in the final summary panel (e.g.
// `tools  pnpm 10.18.1 · go 1.25.0`).
type FinalField struct {
	Label string
	Value string
}

// Reporter is the small event sink both renderers implement.
//
// Implementations must be safe to call from a single goroutine. Pretty
// performs in-place updates, so callers must not interleave raw
// fmt.Fprintln on the same writer between StepStarted and StepFinished.
type Reporter interface {
	// Header prints a one-line header (e.g. `sklp ci · 3 files changed`).
	// Optional; pass "" to skip.
	Header(line string)

	// StepStarted marks `name` as the active step. Pretty shows a
	// spinner; Plain prints "<name>: started".
	StepStarted(name string)

	// StepProgress updates the progress hint on the active step (e.g.
	// "running 87/180 pkg"). Pretty repaints in place; Plain ignores.
	StepProgress(name, hint string)

	// StepFinished freezes `name` with the given status / duration /
	// inline summary (e.g. "2 failed · 178 passed").
	StepFinished(name string, status Status, dur time.Duration, summary string)

	// SubAction prints a dim sub-bullet under the active step (used by
	// the init flow to humanize tool calls).
	SubAction(desc string)

	// Failures prints the FAILURES / REASON section between the step
	// list and the bottom banner.
	Failures(title string, items []Failure)

	// Finalize renders the closing banner with status + total duration
	// + summary fields. After Finalize, no more events should be sent.
	Finalize(title string, status Status, total time.Duration, fields []FinalField, hints []FinalField)
}

// StepStreamer is an optional capability: reporters that implement it
// receive each line of stdout/stderr from a step as it is produced, so
// they can render a live tail under the active step instead of waiting
// for StepFinished. Detected via type assertion at runner wiring time.
//
// The contract is presentation-only: skipping or dropping a StepLine
// call MUST NOT affect the run's outcome or the final log capture. The
// runner already mirrors every line to the per-run log file via the
// existing per-step writer chain — the streamer is purely a UX feed
// layered on top.
type StepStreamer interface {
	// StepLine receives one line (no trailing newline) from a step's
	// stdout or stderr. `stderr` is true when the byte came from the
	// step's stderr pipe.
	//
	// Implementations MUST NOT block the caller: this is wired into the
	// per-step writer chain that feeds the actual command, so a slow
	// reporter would back-pressure the shell. Drop on a full channel
	// rather than waiting.
	StepLine(stepName string, line string, stderr bool)
}

// MultiStepReporter is the optional capability that lets the scheduler
// announce / finish multiple steps concurrently. A reporter that
// implements it accepts overlapping StepStarted / StepFinished calls
// from different goroutines without serialising them externally.
//
// Callers detect support via a type assertion:
//
//	if _, ok := rep.(reporter.MultiStepReporter); ok { … }
//
// The default PlainReporter does NOT implement this — callers must
// keep their existing per-step serialisation when the assertion fails,
// so non-TTY output stays byte-identical.
type MultiStepReporter interface {
	Reporter
	// SupportsMultiStep is a marker; its presence (not its return value)
	// signals the capability. Keeping it as a real method keeps the
	// type-assertion idiomatic.
	SupportsMultiStep()
}

// Options tweak how New() picks a renderer.
type Options struct {
	// ForcePlain disables the pretty renderer regardless of TTY status.
	// Used by --no-color callers, or by test code that captures stdout.
	ForcePlain bool

	// ASCII falls back to ASCII-only symbols. Honour this when the
	// locale is not UTF-8 or the user passes --ascii.
	ASCII bool

	// NoColor disables ANSI sequences but keeps pretty layout (still
	// useful in TTY terminals that don't grok colour).
	NoColor bool

	// TailLines overrides the per-step rolling tail cap. 0 means "auto":
	// the reporter probes the terminal height and picks min(10, h/4)
	// with a floor of 3. Set explicitly by `sklp run --tail-lines N`.
	TailLines int

	// EnableHotkeys arms the raw-mode stdin listener that lets the user
	// grow / shrink / flush the live tail at runtime (+/-/f/q). Honoured
	// only when stdin is a TTY; piped stdin makes this a no-op.
	EnableHotkeys bool
}

// New returns the best reporter for w. Detection rules:
//   - opts.ForcePlain               → PlainReporter
//   - w is not a *os.File           → PlainReporter (pipes, buffers)
//   - !isTerminal(file.Fd())        → PlainReporter
//   - $NO_COLOR set (any value)     → PlainReporter
//   - otherwise                     → PrettyReporter
//
// Honouring $NO_COLOR is the industry standard (see no-color.org); we
// downgrade to PlainReporter rather than emitting bare layout without
// colour because the spinner / in-place updates also rely on ANSI.
func New(w io.Writer, opts Options) Reporter {
	if opts.ForcePlain {
		return &PlainReporter{W: w}
	}
	if os.Getenv("NO_COLOR") != "" {
		return &PlainReporter{W: w}
	}
	f, ok := w.(*os.File)
	if !ok {
		return &PlainReporter{W: w}
	}
	if !isTerminal(f) {
		return &PlainReporter{W: w}
	}
	r := &PrettyReporter{W: w}
	if opts.ASCII {
		r.ascii = true
	}
	if opts.NoColor {
		r.noColor = true
	}
	r.tailCapOverride = opts.TailLines
	if opts.EnableHotkeys && isTerminal(os.Stdin) {
		r.startHotkeyListener()
	}
	return r
}

// FormatDuration returns "1.4s" / "28.4s" / "1m24s" depending on size.
// Public so test fixtures can use the same humanizer.
func FormatDuration(d time.Duration) string {
	if d <= 0 {
		return "0.0s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	m := int(d / time.Minute)
	s := int((d % time.Minute) / time.Second)
	return fmt.Sprintf("%dm%02ds", m, s)
}

// padRight returns s padded to width n with spaces.
func padRight(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}
