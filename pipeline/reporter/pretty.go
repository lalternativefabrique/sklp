package reporter

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// PrettyReporter renders the spec'd layout: heavy/thin banners, colored
// step list, dim sub-actions, final summary panel.
//
// Two rendering modes coexist:
//
//   - Sequential: a single active step at a time. StepStarted prints a
//     "running" line; StepFinished overwrites it with the final glyph
//     by re-emitting a fresh line. This is what `--jobs 1` uses and
//     keeps the engine-2.1 output byte-identical.
//
//   - Live (multi-step): two or more steps are in flight simultaneously.
//     The reporter draws a rolling block of "running" lines at the
//     bottom of the screen and a ticker repaints it ~10 Hz so the
//     spinner spins and the per-step duration counts up. When a step
//     finishes, the block is erased, the finished step is printed as a
//     frozen line above, and the block is redrawn. The frozen log
//     accumulates upward; only the live block is ever in-place.
//
// The transition from sequential to live is automatic: the second
// concurrent StepStarted promotes the reporter into live mode. Falling
// back is one-way per Finalize / clean-up cycle.
//
// All public methods are safe to call from multiple goroutines. The
// MultiStepReporter capability is implemented via a marker method
// (SupportsMultiStep) so the scheduler can detect it via type-assertion.
type PrettyReporter struct {
	W       io.Writer
	ascii   bool
	noColor bool

	mu sync.Mutex

	// Sequential mode bookkeeping (one active step, in-place repaint).
	activeStep string

	// Live mode bookkeeping. live is true once we have entered live
	// mode (>= 2 concurrent steps). active is ordered by start time so
	// repaints stay stable. drawnLines is how many rows the live block
	// occupied last paint so we can erase exactly that many.
	live       bool
	active     []*liveStep
	drawnLines int

	// ticker drives the spinner / duration repaint while live mode is
	// active. Started on the first transition into live; stopped when
	// the last active step finishes.
	ticker     *time.Ticker
	tickerDone chan struct{}

	// spinFrame walks the braille spinner so each repaint shows the
	// next glyph.
	spinFrame int

	// streaming, when true, makes a single concurrent step also render
	// via the live-block path (spinner + rolling tail) instead of the
	// engine-2.1 sequential "running\n…final\n" output. The runner
	// flips this on when it attaches a StepStreamer source. The flag
	// stays sticky for the rest of the run so steps without output
	// also get the spinner-with-duration treatment, matching the
	// multi-step look.
	streaming bool

	// prefixLines, when true, prefixes each tail line with "[step]" so
	// the user can tell which parallel step produced which line. Only
	// useful in multi-step runs; sequential -jobs 1 leaves it false.
	// Set automatically the first time `live && len(active) >= 2`.
	prefixLines bool

	// noPrefixLines, when true, hard-disables the [step] tail prefix even
	// in multi-step mode. Callers that already prefix each line themselves
	// (e.g. `sklp deploy`, which renders `[svc] …` compose-style) set it via
	// DisableLinePrefix to avoid a doubled `[svc] [svc]` prefix.
	noPrefixLines bool

	// termWidth caps the visible width of every row painted inside the
	// live block. Lines that would otherwise wrap onto a second visual
	// row are truncated with an ellipsis so r.drawnLines (a logical
	// line count) stays in sync with the number of visual rows that
	// eraseLiveLocked must rewind over. Probed lazily from the writer
	// when zero; tests override it directly. The value 0 means "no
	// cap" (used when stdout is not a TTY, which is the same path that
	// would route to PlainReporter anyway — kept as a safety net).
	termWidth int

	// termHeight caps the total number of rows painted by the live
	// block. When the active set + tails would exceed this budget, the
	// painter drops oldest tail entries (round-robin across steps) until
	// it fits. This is the #409 spinner-stacking root cause: without a
	// budget, the live block grew past the terminal height, the terminal
	// scrolled, and eraseLiveLocked's cursor-up rewind walked off the
	// top of the visible region — leaving the scrolled-out rows stranded
	// as "frozen" content that looked like duplicate spinners.
	termHeight int

	// tailCapOverride is the user-provided --tail-lines value (0 = auto).
	// effectiveTailCap() resolves the actual per-step tail cap each
	// paint so a SIGWINCH (handled implicitly by the repaint ticker)
	// reflows without restart.
	tailCapOverride int

	// hotkeysOn is set once the raw-mode stdin listener has been
	// successfully armed.
	hotkeysOn   bool
	hotkeysStop func()
}

// EnableStreaming opts the reporter into live tail rendering and starts
// the repaint ticker. Idempotent. Called by the runner once it confirms
// the reporter implements StepStreamer and the user has not passed
// -quiet.
func (r *PrettyReporter) EnableStreaming() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.streaming {
		return
	}
	r.streaming = true
	// If a step is already running in the sequential fast path,
	// promote it into live mode so the upcoming StepLine calls have
	// somewhere to render. The sequential "running" line printed by
	// StepStarted is one row above — erase it before the live block
	// takes over.
	if !r.live && len(r.active) > 0 {
		fmt.Fprint(r.W, ansiCursorUp+ansiEraseLine)
		r.activeStep = ""
		r.live = true
		r.startTickerLocked()
		r.paintLiveLocked()
	}
}

// DisableLinePrefix turns off the automatic "[step]" tail prefix. Used by a
// caller that already prefixes each streamed line itself (so the prefix is not
// doubled). Idempotent and safe to call before or after EnableStreaming.
func (r *PrettyReporter) DisableLinePrefix() {
	r.mu.Lock()
	r.noPrefixLines = true
	r.mu.Unlock()
}

type liveStep struct {
	name  string
	start time.Time
	// tail holds the recent stream lines for this step. The painter
	// shows at most effectiveTailCap() of them per step; entries beyond
	// the cap are kept in memory so that a "f"-hotkey flush can still
	// surface them in one frame. Capped at flushAllMax to bound memory.
	tail []string
	// flushAll is the per-step "show me everything" toggle wired to the
	// 'f' hotkey. The next paint frame inlines every captured line under
	// the spinner row. 'q' clears it back.
	flushAll bool
}

// tailCapDefault is the per-step tail cap when the terminal height is
// unknown (non-TTY safety net, or term.GetSize failure). Five lines is
// enough to show "something is happening" without overwhelming the
// screen when many steps run in parallel. The auto formula
// `min(10, height/4)` resolves to a value <= 10 on real terminals.
const tailCapDefault = 5

// tailCapAutoMax is the ceiling the smart-default formula will not
// exceed even on a very tall terminal — beyond this each step's tail
// drowns the rest of the live block.
const tailCapAutoMax = 10

// tailCapFloor is the floor below which a value of TailLines is
// silently clamped. One line is the minimum useful tail (the user can
// always still see "something is happening" via the spinner).
const tailCapFloor = 1

// flushAllMax bounds the per-step buffered tail. Beyond this we drop
// the oldest line even though it would never have rendered — keeps
// memory predictable on a multi-hour `pnpm install` that prints tens
// of thousands of lines.
const flushAllMax = 5000

// maxLineWidth bounds the rendered width of a streamed line so it can
// never wrap and corrupt the in-place live block. Anything longer is
// truncated with a trailing ellipsis. The value is a conservative
// fallback for terminals that don't expose a width — pretty enough for
// 80-column / 120-column setups without depending on isatty width.
const maxLineWidth = 160

// SupportsMultiStep marks PrettyReporter as a MultiStepReporter.
func (r *PrettyReporter) SupportsMultiStep() {}

// ANSI palette. Kept colocated so a designer changing the look only
// touches one block.
const (
	colReset = "\033[0m"
	colBold  = "\033[1m"
	colDim   = "\033[2m"
	colRed   = "\033[31m"
	colGreen = "\033[32m"
	colYel   = "\033[33m"
	colCyan  = "\033[36m"
	colGray  = "\033[90m"

	// ANSI control sequences for the live block.
	ansiCursorUp   = "\033[1A"
	ansiEraseLine  = "\033[2K\r"
	ansiEraseDown  = "\033[0J" // clear from cursor to end of screen
	ansiHideCursor = "\033[?25l"
	ansiShowCursor = "\033[?25h"
)

// spinnerFrames cycles through the standard braille spinner. ASCII
// fallback substitutes a single bar so the layout width stays the
// same.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func (r *PrettyReporter) c(code, s string) string {
	if r.noColor || code == "" {
		return s
	}
	return code + s + colReset
}

// glyph returns the symbol for a status. ASCII fallback matches the spec
// table ([OK] / [X] / [-] / |).
func (r *PrettyReporter) glyph(s Status) string {
	if r.ascii {
		switch s {
		case StatusPassed:
			return "[OK]"
		case StatusFailed:
			return "[X]"
		case StatusSkipped:
			return "[-]"
		case StatusRunning:
			return "|"
		case StatusCached:
			return "[~]"
		}
		return " "
	}
	switch s {
	case StatusPassed:
		return "✓"
	case StatusFailed:
		return "✗"
	case StatusSkipped:
		return "⊘"
	case StatusRunning:
		return spinnerFrames[r.spinFrame%len(spinnerFrames)]
	case StatusCached:
		return "↺"
	}
	return " "
}

// colorFor returns the ANSI code conventionally paired with a status.
func (r *PrettyReporter) colorFor(s Status) string {
	switch s {
	case StatusPassed:
		return colGreen
	case StatusFailed:
		return colRed
	case StatusSkipped:
		return colGray
	case StatusRunning:
		return colCyan
	case StatusCached:
		return colCyan
	}
	return ""
}

// width is the target content column for the right-aligned duration.
const width = 52

func (r *PrettyReporter) Header(line string) {
	if line == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.eraseLiveLocked()
	fmt.Fprintln(r.W)
	fmt.Fprintf(r.W, "  %s\n", r.c(colDim, line))
	fmt.Fprintln(r.W)
	r.paintLiveLocked()
}

func (r *PrettyReporter) StepStarted(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Sequential-mode fast path: nothing currently active. Match the
	// engine-2.1 output exactly so --jobs 1 stays byte-identical when
	// streaming is off (i.e. -quiet).
	if !r.live && !r.streaming && len(r.active) == 0 {
		r.activeStep = name
		fmt.Fprintf(r.W, "  %s %s\n",
			r.c(colCyan, r.glyph(StatusRunning)),
			name,
		)
		r.active = append(r.active, &liveStep{name: name, start: time.Now()})
		return
	}

	// Streaming enabled: a single step still renders via the live
	// block so its rolling tail (StepLine calls) has a stable place
	// to paint.
	if !r.live && r.streaming {
		r.live = true
		r.startTickerLocked()
		r.active = append(r.active, &liveStep{name: name, start: time.Now()})
		r.paintLiveLocked()
		return
	}

	// A second concurrent start promotes the reporter into live mode.
	// The previously-printed sequential "running" line gets erased and
	// re-folded into the live block so the user sees both spinners in
	// one place.
	if !r.live {
		// Erase the sequential running line that StepStarted just
		// emitted for the first step (one line up).
		fmt.Fprint(r.W, ansiCursorUp+ansiEraseLine)
		r.activeStep = ""
		r.live = true
		r.startTickerLocked()
	}
	r.active = append(r.active, &liveStep{name: name, start: time.Now()})
	if len(r.active) >= 2 {
		r.prefixLines = true
	}
	r.repaintLiveLocked()
}

func (r *PrettyReporter) StepProgress(name, hint string) {
	// Progress hints clobber past lines in sequential mode and would
	// race against the live block in multi-step mode. The init flow
	// already uses SubAction for the same purpose, so we degrade to a
	// no-op when live and to a dim sub-bullet when sequential — the
	// pre-2.5 behaviour.
	if hint == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live {
		return
	}
	fmt.Fprintf(r.W, "    %s\n", r.c(colDim, "· "+hint))
}

func (r *PrettyReporter) StepFinished(name string, status Status, dur time.Duration, summary string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Pull the step out of the active list (if present). The reporter
	// tolerates a finished-without-started call so callers don't have to
	// pair them defensively (e.g. compile-skipped steps).
	for i, s := range r.active {
		if s.name == name {
			r.active = append(r.active[:i], r.active[i+1:]...)
			break
		}
	}

	if r.live {
		// Erase the live block, print the frozen line, redraw.
		r.eraseLiveLocked()
		r.writeFrozenLineLocked(name, status, dur, summary)
		if len(r.active) == 0 {
			r.stopTickerLocked()
			r.live = false
			r.drawnLines = 0
			return
		}
		r.paintLiveLocked()
		return
	}

	// Sequential mode: emit the final line below the "running" line.
	// We deliberately keep the running line on screen so the byte
	// output is identical to the engine-2.1 (--jobs 1) format —
	// downstream snapshot tests rely on this.
	if r.activeStep == name {
		r.activeStep = ""
	}
	r.writeFrozenLineLocked(name, status, dur, summary)
}

func (r *PrettyReporter) SubAction(desc string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.eraseLiveLocked()
	fmt.Fprintf(r.W, "    %s\n", r.c(colDim, "· "+desc))
	r.paintLiveLocked()
}

func (r *PrettyReporter) Failures(title string, items []Failure) {
	if len(items) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.eraseLiveLocked()
	defer r.paintLiveLocked()

	sep := strings.Repeat("─", 53)
	if r.ascii {
		sep = strings.Repeat("-", 53)
	}
	fmt.Fprintln(r.W)
	fmt.Fprintf(r.W, "  %s\n", r.c(colGray, sep))
	fmt.Fprintf(r.W, "   %s\n", r.c(colDim, title))
	fmt.Fprintf(r.W, "  %s\n", r.c(colGray, sep))
	fmt.Fprintln(r.W)
	for _, f := range items {
		head := f.Title
		if f.Scope != "" {
			arrow := "›"
			if r.ascii {
				arrow = ">"
			}
			head = fmt.Sprintf("%s %s %s", f.Scope, r.c(colGray, arrow), f.Title)
		}
		fmt.Fprintf(r.W, "   %s %s\n", r.c(colRed, r.glyph(StatusFailed)), head)
		if f.Message != "" {
			fmt.Fprintln(r.W)
			if f.PreserveBytes {
				// Byte-faithful path for ASCII tables (Trivy, etc.):
				// each captured line is emitted with a uniform two-space
				// indent and NO further width/column manipulation. The
				// indent is small enough that columns the tool baked
				// into the table at COLUMNS=80 still line up visually.
				// #440: the previous path indented 7 spaces AND used
				// strings.Split on already-truncated lines, producing
				// the column-of-empty-`|` artefact.
				for _, ln := range strings.Split(strings.TrimRight(f.Message, "\n"), "\n") {
					if ln == "" {
						fmt.Fprintln(r.W)
						continue
					}
					// Hyperlink CVE URLs that Trivy embeds in cells —
					// #444 phase C. Non-OSC8 terminals just see the
					// original URL text inside.
					decorated := decorateCVEURLs(r, ln)
					fmt.Fprintf(r.W, "  %s\n", decorated)
				}
			} else {
				for _, ln := range strings.Split(strings.TrimRight(f.Message, "\n"), "\n") {
					fmt.Fprintf(r.W, "       %s\n", ln)
				}
			}
		}
		if f.Location != "" {
			fmt.Fprintf(r.W, "       %s\n", r.c(colDim, f.Location))
		}
		fmt.Fprintln(r.W)
	}
}

func (r *PrettyReporter) Finalize(title string, status Status, total time.Duration, fields []FinalField, hints []FinalField) {
	r.mu.Lock()
	stopHotkeys := r.hotkeysStop
	r.hotkeysStop = nil
	r.hotkeysOn = false
	r.stopTickerLocked()
	r.eraseLiveLocked()
	r.live = false
	r.drawnLines = 0
	r.mu.Unlock()
	if stopHotkeys != nil {
		stopHotkeys()
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	// Decorate the `log` field with an OSC 8 hyperlink so terminals
	// that grok it turn the path into a click-thru. We mutate a copy so
	// callers' slices stay untouched. #444 phase C.
	fields = decorateLogField(r, fields)

	bar := strings.Repeat("━", 53)
	if r.ascii {
		bar = strings.Repeat("=", 53)
	}
	color := r.colorFor(status)
	glyph := r.glyph(status)
	verdict := "PASSED"
	if status == StatusFailed {
		verdict = "FAILED"
	}
	header := fmt.Sprintf("%s  %s", glyph, title+" "+verdict)
	durStr := FormatDuration(total)

	fmt.Fprintln(r.W)
	fmt.Fprintf(r.W, "  %s\n", r.c(color, bar))
	// Bold only on the verdict line, per spec.
	left := r.c(colBold, r.c(color, header))
	visible := glyph + "  " + title + " " + verdict
	pad := width - utf8.RuneCountInString(visible) - utf8.RuneCountInString(durStr)
	if pad < 1 {
		pad = 1
	}
	fmt.Fprintf(r.W, "   %s%s%s\n", left, strings.Repeat(" ", pad), durStr)
	fmt.Fprintf(r.W, "  %s\n", r.c(color, bar))
	if len(fields) > 0 {
		fmt.Fprintln(r.W)
		for _, f := range fields {
			fmt.Fprintf(r.W, "   %s   %s\n", padRight(r.c(colDim, f.Label), 10+len(colDim)+len(colReset)), f.Value)
		}
	}
	if len(hints) > 0 {
		fmt.Fprintln(r.W)
		for _, h := range hints {
			fmt.Fprintf(r.W, "   %s   %s\n", padRight(r.c(colDim, h.Label), 10+len(colDim)+len(colReset)), r.c(colDim, h.Value))
		}
	}
	fmt.Fprintln(r.W)
}

// writeFrozenLineLocked prints a completed step line in the scroll
// region (above any future live block). Caller holds r.mu.
//
// The composed line is truncated to currentWidth() before printing.
// Without that clamp, on narrow terminals (<80 cols) the line wraps to
// two visual rows while the writer accounts for a single logical row.
// Subsequent eraseLiveLocked rewinds then walk short and the next
// paint stacks on top of the stranded continuation, producing the
// cascading-spinner pile users see during long parallel runs.
func (r *PrettyReporter) writeFrozenLineLocked(name string, status Status, dur time.Duration, summary string) {
	left := fmt.Sprintf("%s %s", r.c(r.colorFor(status), r.glyph(status)), name)
	if summary != "" {
		left = fmt.Sprintf("%s  %s", left, r.c(colYel, summary))
	}
	durStr := r.c(colDim, FormatDuration(dur))
	visible := r.glyph(status) + " " + name
	if summary != "" {
		visible += "  " + summary
	}
	pad := width - utf8.RuneCountInString(visible)
	if pad < 1 {
		pad = 1
	}
	row := fmt.Sprintf("  %s%s%s", left, strings.Repeat(" ", pad), durStr)
	row = truncateVisible(row, r.currentWidth())
	fmt.Fprintln(r.W, row)
}

// paintLiveLocked draws the live block at the current cursor position
// and records its height in r.drawnLines so eraseLiveLocked can rewind
// exactly. Every row written here is truncated to r.currentWidth() so
// the terminal cannot auto-wrap a row onto two visual lines — wrapping
// would desync r.drawnLines from the cursor's real position and cause
// each subsequent paint to append rather than overwrite. Caller holds
// r.mu.
func (r *PrettyReporter) paintLiveLocked() {
	if !r.live || len(r.active) == 0 {
		r.drawnLines = 0
		return
	}
	w := r.currentWidth()
	perStepCap := r.effectiveTailCapLocked()
	// Resolve a per-step slice of the tail that will actually render.
	// flushAll bypasses the cap (the user explicitly asked for the
	// whole thing). The total-row budget below may still trim it.
	tailSlices := make([][]string, len(r.active))
	totalRows := 0
	for i, s := range r.active {
		t := s.tail
		if !s.flushAll && len(t) > perStepCap {
			t = t[len(t)-perStepCap:]
		}
		tailSlices[i] = t
		totalRows += 1 + len(t) // spinner row + tail rows
	}

	// Budget against the terminal height. We reserve a small headroom
	// so finished-line writes, banners, and the cursor itself have room
	// to breathe between repaints — the live block never owns the whole
	// screen. Drop oldest tail entries cross-step, round-robin, until
	// the painted block fits the budget. This is the #409 fix: without
	// budgeting, the block exceeded the terminal height, the terminal
	// scrolled, and eraseLiveLocked rewound past the visible region.
	if budget := r.tailBudgetLocked(); budget > 0 && totalRows > budget {
		excess := totalRows - budget
		// Round-robin drop oldest tail entries across the active steps.
		// Stops short of removing the spinner row itself: we always
		// leave at least the spinner so the user sees the step is alive.
		for excess > 0 {
			progressed := false
			for i := range tailSlices {
				if excess == 0 {
					break
				}
				if len(tailSlices[i]) > 0 {
					tailSlices[i] = tailSlices[i][1:]
					excess--
					progressed = true
				}
			}
			if !progressed {
				// All tails empty; the active set itself exceeds the
				// budget. Truncate the active step list visually — the
				// excess steps still run, they just don't paint a
				// spinner this frame. Acceptable degraded display.
				break
			}
		}
	}

	drawn := 0
	budget := r.tailBudgetLocked()
	for i, s := range r.active {
		if budget > 0 && drawn >= budget {
			break
		}
		dur := time.Since(s.start)
		glyph := r.glyph(StatusRunning)
		durStr := FormatDuration(dur)
		stepHead := glyph + " " + s.name
		pad := width - utf8.RuneCountInString(stepHead)
		if pad < 1 {
			pad = 1
		}
		visible := "  " + stepHead + strings.Repeat(" ", pad) + durStr
		visible = truncateVisible(visible, w)
		// Re-apply colour to the parts that need it. Splitting after
		// truncation keeps the cell count exact: ANSI escapes contain
		// zero printable cells, so the row width stays == len(visible).
		fmt.Fprintln(r.W, colorizeSpinnerRow(r, visible, glyph, durStr))
		drawn++
		for _, ln := range tailSlices[i] {
			if budget > 0 && drawn >= budget {
				break
			}
			body := ln
			if r.prefixLines && !r.noPrefixLines {
				body = "[" + s.name + "] " + ln
			}
			row := "    " + body
			row = truncateVisible(row, w)
			fmt.Fprintf(r.W, "%s\n", r.c(colDim, row))
			drawn++
		}
	}
	r.drawnLines = drawn
}

// effectiveTailCapLocked returns the per-step rolling tail cap to use
// for the next paint. Resolution order:
//
//  1. tailCapOverride (--tail-lines flag, hotkey ±), clamped to >=
//     tailCapFloor.
//  2. Smart default: min(tailCapAutoMax, termHeight/4). The /4 ratio
//     leaves room for finished-step lines + the final banner.
//  3. tailCapDefault when termHeight is unknown.
//
// Caller holds r.mu.
func (r *PrettyReporter) effectiveTailCapLocked() int {
	if r.tailCapOverride > 0 {
		if r.tailCapOverride < tailCapFloor {
			return tailCapFloor
		}
		return r.tailCapOverride
	}
	h := r.currentHeight()
	if h <= 0 {
		return tailCapDefault
	}
	cap := h / 4
	if cap > tailCapAutoMax {
		cap = tailCapAutoMax
	}
	if cap < tailCapFloor {
		cap = tailCapFloor
	}
	return cap
}

// tailBudgetLocked returns the maximum total rows the live block may
// occupy. termHeight - 4 (one for prompt, one safety) clamped to >= 4.
// Returns 0 when the height is unknown — callers must skip budgeting
// in that case (degraded-open: paint everything, same as pre-#444).
// Caller holds r.mu.
func (r *PrettyReporter) tailBudgetLocked() int {
	h := r.currentHeight()
	if h <= 0 {
		return 0
	}
	b := h - 4
	if b < 4 {
		b = 4
	}
	return b
}

// currentWidth returns the column cap used to clamp every live-block
// row. Probed once from the writer when zero; falls back to
// maxLineWidth (the legacy 160-col cap) when no fd is available.
func (r *PrettyReporter) currentWidth() int {
	if r.termWidth > 0 {
		return r.termWidth
	}
	if f, ok := r.W.(*os.File); ok {
		if cols, rows, err := term.GetSize(int(f.Fd())); err == nil && cols > 0 {
			r.termWidth = cols
			if rows > 0 && r.termHeight == 0 {
				r.termHeight = rows
			}
			return cols
		}
	}
	return maxLineWidth
}

// currentHeight mirrors currentWidth for row budgeting. Returns 0 when
// the height is unknown (non-TTY or probe failure) — callers must
// interpret 0 as "no budget".
func (r *PrettyReporter) currentHeight() int {
	if r.termHeight > 0 {
		return r.termHeight
	}
	if f, ok := r.W.(*os.File); ok {
		if cols, rows, err := term.GetSize(int(f.Fd())); err == nil && rows > 0 {
			r.termHeight = rows
			if cols > 0 && r.termWidth == 0 {
				r.termWidth = cols
			}
			return rows
		}
	}
	return 0
}

// truncateVisible returns s clipped so its visible cell count is at
// most max-1 (one cell of safety margin so terminals that wrap on the
// trailing column never bite us). ANSI sequences are not expected in
// the input — callers must build the plain row first, truncate, then
// re-colourise.
func truncateVisible(s string, max int) string {
	if max <= 0 {
		return s
	}
	limit := max - 1
	if limit <= 0 {
		limit = 1
	}
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	// Reserve one cell for the ellipsis.
	keep := limit - 1
	if keep < 0 {
		keep = 0
	}
	count := 0
	for i := range s {
		if count == keep {
			return s[:i] + "…"
		}
		count++
	}
	return s
}

// colorizeSpinnerRow restores ANSI colours on the truncated spinner
// row. visible is the plain truncated row; glyph and durStr are the
// pre-truncation tokens we want to highlight if they still appear.
// When truncation has consumed the duration token we just emit the
// row as plain (still legible — losing colour on an extreme corner
// case beats corrupting the live block).
func colorizeSpinnerRow(r *PrettyReporter, visible, glyph, durStr string) string {
	gi := strings.Index(visible, glyph)
	di := strings.LastIndex(visible, durStr)
	if gi < 0 {
		return visible
	}
	out := visible[:gi] + r.c(colCyan, glyph)
	if di > gi {
		out += visible[gi+len(glyph):di] + r.c(colDim, durStr) + visible[di+len(durStr):]
	} else {
		out += visible[gi+len(glyph):]
	}
	return out
}

// StepLine appends a streamed output line to the named step's rolling
// tail and triggers an in-place repaint. The line is truncated to
// maxLineWidth so it can never wrap and corrupt the live block.
//
// Lines for steps that are not currently active (already finished, or
// never started) are dropped silently — they would have nowhere to
// render. Same goes for calls that arrive before EnableStreaming or
// after Finalize.
func (r *PrettyReporter) StepLine(stepName string, line string, _ bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.live || len(r.active) == 0 {
		return
	}
	var target *liveStep
	for _, s := range r.active {
		if s.name == stepName {
			target = s
			break
		}
	}
	if target == nil {
		return
	}
	if utf8.RuneCountInString(line) > maxLineWidth {
		// Rune-safe truncation so a multi-byte rune at the cap boundary
		// does not get sliced mid-sequence — important for the box-
		// drawing / CJK characters Trivy tables and `pnpm install`
		// occasionally emit.
		line = truncateVisible(line, maxLineWidth+1)
	}
	target.tail = append(target.tail, line)
	if len(target.tail) > flushAllMax {
		target.tail = target.tail[len(target.tail)-flushAllMax:]
	}
	r.repaintLiveLocked()
}

// eraseLiveLocked rewinds the cursor over the last live-block paint
// and clears each row so the next write lands on the row where the
// block started. Caller holds r.mu.
//
// The walk-up-and-erase loop assumes each logical row painted last
// frame occupies exactly one terminal row. That holds for the spinner
// row (always truncated to currentWidth()) and for tail rows fed
// through StepLine (also truncated). But the same writer also carries
// bytes that bypass the paint path entirely — sub-process stdout
// routed through LineTee.Underlying, banners, frozen lines. If any of
// those exceeded the terminal width without our knowing, the terminal
// auto-wrapped them onto two visual rows, drawnLines under-counts
// reality, the rewind walks short, and the next paint stacks on top
// of the stranded residue. That is the duplicated-step-name pile
// users see during long parallel runs (sklp issue: rolling tail
// mutates into duplicated step names when text scrolls).
//
// After walking up `drawnLines` rows we therefore also issue a
// "erase from cursor to end of screen" so any residue below (wrapped
// continuations, partial repaints from a prior aborted frame) is
// wiped before the next paint runs. The CUU+EL loop above already
// erases the line we landed on, so the extra EL-down only affects
// rows beneath it.
func (r *PrettyReporter) eraseLiveLocked() {
	for i := 0; i < r.drawnLines; i++ {
		fmt.Fprint(r.W, ansiCursorUp+ansiEraseLine)
	}
	if r.drawnLines > 0 {
		fmt.Fprint(r.W, ansiEraseDown)
	}
	r.drawnLines = 0
}

// repaintLiveLocked erases + repaints the block in place. Used by the
// ticker and by mutations to the active set. Caller holds r.mu.
func (r *PrettyReporter) repaintLiveLocked() {
	r.eraseLiveLocked()
	r.paintLiveLocked()
}

// startTickerLocked starts the spinner / duration repaint goroutine.
// Idempotent. Caller holds r.mu.
func (r *PrettyReporter) startTickerLocked() {
	if r.ticker != nil {
		return
	}
	r.ticker = time.NewTicker(100 * time.Millisecond)
	r.tickerDone = make(chan struct{})
	done := r.tickerDone
	tk := r.ticker
	go func() {
		for {
			select {
			case <-done:
				return
			case <-tk.C:
				r.mu.Lock()
				if !r.live {
					r.mu.Unlock()
					return
				}
				r.spinFrame++
				r.repaintLiveLocked()
				r.mu.Unlock()
			}
		}
	}()
	// Resize handling: the ticker redraws every ~100 ms, so a SIGWINCH
	// is absorbed within a frame without dedicated signal handling.
}

// stopTickerLocked halts the repaint goroutine. Idempotent. Caller
// holds r.mu.
func (r *PrettyReporter) stopTickerLocked() {
	if r.ticker == nil {
		return
	}
	r.ticker.Stop()
	close(r.tickerDone)
	r.ticker = nil
	r.tickerDone = nil
}

// decorateLogField returns a copy of `fields` where the entry whose
// Label is "log" / "full logs" has its Value wrapped in an OSC 8
// hyperlink to file://<abs-path>. Other fields pass through unchanged.
// The wrap is skipped when the value doesn't look like an absolute
// path (defensive: prevents emitting a malformed file:// URL).
func decorateLogField(r *PrettyReporter, fields []FinalField) []FinalField {
	if len(fields) == 0 || r.noColor {
		return fields
	}
	out := make([]FinalField, len(fields))
	copy(out, fields)
	for i, f := range out {
		if f.Label != "log" && f.Label != "full logs" {
			continue
		}
		if !strings.HasPrefix(f.Value, "/") {
			continue
		}
		out[i].Value = r.hyperlink("file://"+f.Value, f.Value)
	}
	return out
}

// hyperlink wraps `text` in an OSC 8 escape sequence so a modern
// terminal (iTerm2, kitty, wezterm, alacritty, vscode, gnome-terminal,
// recent Windows Terminal, …) renders it as a clickable link to `url`.
// Terminals without OSC 8 support display `text` unchanged.
//
// When the reporter is in --no-color mode (or stdout is not a TTY,
// though New() routes that to PlainReporter anyway) we skip the wrap
// to keep snapshot output stable and copy-paste-clean.
//
// Format: ESC ] 8 ; ; URL BEL TEXT ESC ] 8 ; ; BEL  (BEL = \x07)
// https://gist.github.com/egmontkob/eb114294efbcd5adb1944c9f3cb5feda
func (r *PrettyReporter) hyperlink(url, text string) string {
	if r.noColor || url == "" {
		return text
	}
	return "\033]8;;" + url + "\007" + text + "\033]8;;\007"
}

// cveURLRE matches the Aqua AVD URLs Trivy embeds in its HIGH/CRITICAL
// rows. Kept narrow on purpose — we only decorate URLs the user is
// actually likely to click; matching every http(s):// would risk
// breaking already-shaped table columns when the URL is itself the
// padding source.
var cveURLRE = regexp.MustCompile(`https://avd\.aquasec\.com/[^\s|]+`)

// decorateCVEURLs replaces each occurrence of a CVE URL in `line` with
// the OSC 8 wrapped form. Width is preserved because the OSC 8 wrapper
// has zero printable cells — the table layout stays intact.
func decorateCVEURLs(r *PrettyReporter, line string) string {
	if r.noColor {
		return line
	}
	return cveURLRE.ReplaceAllStringFunc(line, func(u string) string {
		return r.hyperlink(u, u)
	})
}

// startHotkeyListener arms a goroutine that puts os.Stdin in raw mode
// and watches for +/-/f/q keypresses while the live reporter is
// active. Mutations are applied under r.mu so the next repaint frame
// reflects the change. The listener self-terminates when Finalize
// stops the ticker (via hotkeysStop).
//
// Safety:
//   - Honoured only when stdin is a TTY (the caller already checked,
//     but we re-check defensively so the public API stays robust).
//   - The previous terminal state is restored on shutdown so the user
//     gets a working shell back even if the run crashes.
//   - A single goroutine is spawned per reporter — guarded by
//     hotkeysOn so EnableStreaming-then-New-again can't double-arm.
func (r *PrettyReporter) startHotkeyListener() {
	r.mu.Lock()
	if r.hotkeysOn {
		r.mu.Unlock()
		return
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		r.mu.Unlock()
		return
	}
	// Don't use term.MakeRaw: it clears OPOST on the termios, which is
	// shared between stdin and stdout when both point at the same tty.
	// Without OPOST, every "\n" we emit (Fprintln / Fprintf) moves the
	// cursor down without returning to column 0 — the live block then
	// staircases diagonally across the screen. Instead, snapshot the
	// current termios and clear only the input-side flags we need
	// (canonical mode, echo, signal generation, input post-processing)
	// while leaving Oflag untouched so output remains cooked.
	state, err := term.GetState(fd)
	if err != nil {
		r.mu.Unlock()
		return
	}
	t, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		r.mu.Unlock()
		return
	}
	raw := *t
	raw.Lflag &^= unix.ICANON | unix.ECHO | unix.ISIG | unix.IEXTEN
	raw.Iflag &^= unix.IXON | unix.ICRNL | unix.BRKINT | unix.INPCK | unix.ISTRIP
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &raw); err != nil {
		r.mu.Unlock()
		return
	}
	r.hotkeysOn = true
	stop := make(chan struct{})
	r.hotkeysStop = func() {
		select {
		case <-stop:
		default:
			close(stop)
		}
		_ = term.Restore(fd, state)
	}
	r.mu.Unlock()

	go func() {
		// 8 bytes is enough for any escape sequence we care about
		// (CSI arrows are 3 bytes; reserve headroom for future bindings).
		buf := make([]byte, 8)
		for {
			select {
			case <-stop:
				return
			default:
			}
			n, err := os.Stdin.Read(buf)
			if err != nil || n == 0 {
				// EOF / closed stdin / signal: exit cleanly.
				return
			}
			// Detect arrow keys: ESC [ A (up) / ESC [ B (down). Terminals
			// deliver the whole sequence in one read in raw mode, so a
			// single Read with n>=3 starting with 0x1b 0x5b is the common
			// path. A lone 0x1b (user pressed Esc) drops to default.
			var action byte
			switch {
			case n >= 3 && buf[0] == 0x1b && buf[1] == '[':
				switch buf[2] {
				case 'C':
					action = '+' // right arrow = more tail
				case 'D':
					action = '-' // left arrow = less tail
				}
			default:
				action = buf[0]
			}
			r.mu.Lock()
			switch action {
			case '+', '=':
				// '=' so users don't have to press Shift on US layouts;
				// up-arrow also maps here.
				cur := r.effectiveTailCapLocked()
				r.tailCapOverride = cur + 5
			case '-':
				cur := r.effectiveTailCapLocked()
				next := cur - 5
				if next < tailCapFloor {
					next = tailCapFloor
				}
				r.tailCapOverride = next
			case 'f', 'F':
				for _, s := range r.active {
					s.flushAll = true
				}
			case 'q', 'Q':
				for _, s := range r.active {
					s.flushAll = false
				}
				// Reset to auto so the smart default reflows.
				r.tailCapOverride = 0
			case 0x03:
				// Ctrl-C: in raw mode the kernel does NOT translate the
				// byte into SIGINT — it just hands us the 0x03 byte.
				// Returning from this goroutine without doing anything
				// else leaves the runner running, the terminal still in
				// raw mode, and the user staring at a frozen screen.
				//
				// Three things must happen, in order:
				//
				//   1. Stop the repaint ticker so we don't keep painting
				//      over the shutdown messages the runner is about to
				//      emit.
				//   2. Drop raw mode so the next paint/finalize lands in
				//      a normal cooked terminal (and so subsequent Ctrl-C
				//      presses, e.g. while child processes wind down,
				//      reach the kernel signal path again).
				//   3. Deliver a real SIGINT to the runner process so its
				//      context tree is cancelled and child commands get
				//      torn down through the standard signal path.
				stopHotkeys := r.hotkeysStop
				r.hotkeysStop = nil
				r.hotkeysOn = false
				r.stopTickerLocked()
				r.mu.Unlock()
				if stopHotkeys != nil {
					stopHotkeys()
				}
				_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
				return
			default:
				r.mu.Unlock()
				continue
			}
			if r.live {
				r.repaintLiveLocked()
			}
			r.mu.Unlock()
		}
	}()
}
