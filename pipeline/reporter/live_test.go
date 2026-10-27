package reporter

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// TestPrettyReporter_SequentialBytesUnchanged locks the engine-2.1 byte
// shape for the single-active-step path so `--jobs 1` cannot regress.
// The test deliberately reads characters (not lines) because the live
// reporter's ANSI repaints would otherwise be invisible against a
// line-based comparison.
func TestPrettyReporter_SequentialBytesUnchanged(t *testing.T) {
	var buf bytes.Buffer
	r := &PrettyReporter{W: &buf, noColor: true}
	r.StepStarted("tools")
	r.StepFinished("tools", StatusPassed, 400*time.Millisecond, "")
	r.StepStarted("go-test")
	r.StepFinished("go-test", StatusPassed, 24100*time.Millisecond, "180 passed")

	out := buf.String()
	// Sequential mode must emit two lines per step: a running line then
	// a final line. No ANSI cursor-up sequences (those are reserved for
	// live mode).
	if strings.Contains(out, ansiCursorUp) {
		t.Fatalf("sequential output contains ANSI cursor moves (live-mode leakage):\n%q", out)
	}
	for _, want := range []string{
		"⠋ tools",
		"✓ tools",
		"0.4s",
		"⠋ go-test",
		"✓ go-test",
		"180 passed",
		"24.1s",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("sequential output missing %q\n--- got:\n%s", want, out)
		}
	}
}

// TestPrettyReporter_LivePromotion checks that a second concurrent
// StepStarted promotes the reporter into live mode: ANSI cursor moves
// appear, both step names are visible in the live block, and a
// StepFinished call freezes one without erasing the other.
func TestPrettyReporter_LivePromotion(t *testing.T) {
	var buf bytes.Buffer
	r := &PrettyReporter{W: &buf, noColor: true}
	r.StepStarted("a")
	r.StepStarted("b")
	if !r.isLiveForTest() {
		t.Fatalf("second StepStarted did not promote reporter to live mode")
	}
	out := buf.String()
	if !strings.Contains(out, ansiCursorUp) {
		t.Errorf("live mode emitted no ANSI cursor-up sequences:\n%q", out)
	}
	if !strings.Contains(out, " a") || !strings.Contains(out, " b") {
		t.Errorf("live block missing one of the step names:\n%s", out)
	}

	// Stop the ticker so the test doesn't race against repaints while
	// we read the buffer.
	r.mu.Lock()
	r.stopTickerLocked()
	r.mu.Unlock()

	r.StepFinished("a", StatusPassed, time.Second, "")
	out = buf.String()
	if !strings.Contains(out, "✓ a") {
		t.Errorf("frozen line for finished step missing:\n%s", out)
	}
	// 'b' is still active so it must appear at least twice (initial
	// paint + redraw after a's freeze).
	if strings.Count(out, " b") < 2 {
		t.Errorf("active step b should be repainted; got %d occurrences\n%s",
			strings.Count(out, " b"), out)
	}
}

// TestPrettyReporter_FinalizeStopsTicker guards against the live-mode
// ticker outliving the run. After Finalize the goroutine must exit so
// the process can shut down cleanly.
func TestPrettyReporter_FinalizeStopsTicker(t *testing.T) {
	var buf bytes.Buffer
	r := &PrettyReporter{W: &buf, noColor: true}
	r.StepStarted("a")
	r.StepStarted("b")
	r.StepFinished("a", StatusPassed, time.Second, "")
	r.Finalize("CI", StatusPassed, 2*time.Second, nil, nil)
	if r.ticker != nil {
		t.Errorf("Finalize did not stop the ticker")
	}
}

// TestPrettyReporter_ConcurrentSafe is a smoke test for the mutex
// discipline. Two goroutines hammer Start/Finish in tight loops with
// the race detector enabled (via `go test -race`).
func TestPrettyReporter_ConcurrentSafe(t *testing.T) {
	var buf bytes.Buffer
	r := &PrettyReporter{W: &buf, noColor: true}

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				name := stepName(id, i)
				r.StepStarted(name)
				r.StepFinished(name, StatusPassed, time.Millisecond, "")
			}
		}(w)
	}
	wg.Wait()
	r.Finalize("CI", StatusPassed, time.Second, nil, nil)
}

func stepName(worker, i int) string {
	return "s" + intToStr(worker) + "-" + intToStr(i)
}

func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	out := ""
	for n > 0 {
		out = string(rune('0'+n%10)) + out
		n /= 10
	}
	return out
}

// TestMultiStepReporter_TypeAssertion checks the capability detection
// callers rely on. PlainReporter must NOT advertise it; PrettyReporter
// must.
func TestMultiStepReporter_TypeAssertion(t *testing.T) {
	var plain Reporter = &PlainReporter{W: &bytes.Buffer{}}
	if _, ok := plain.(MultiStepReporter); ok {
		t.Errorf("PlainReporter must not advertise MultiStepReporter")
	}
	var pretty Reporter = &PrettyReporter{W: &bytes.Buffer{}}
	if _, ok := pretty.(MultiStepReporter); !ok {
		t.Errorf("PrettyReporter must advertise MultiStepReporter")
	}
}

// isLiveForTest exposes the private live flag to the test suite
// without widening the public API. Test-only.
func (r *PrettyReporter) isLiveForTest() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.live
}

// renderVirtualTerminal replays the ANSI byte stream produced by the
// reporter into an in-memory N-column screen and returns the final
// visible rows (trailing blanks trimmed). Only the subset of sequences
// the reporter actually emits is implemented:
//
//   - `\e[1A`        cursor up one row
//   - `\e[2K`        erase entire current row
//   - `\r`           carriage return (col -> 0)
//   - `\n`           line feed (row -> row+1, scroll if needed)
//   - `\e[<...>m`    SGR — consumed and discarded (colours)
//   - `\e[?25l/h`    show/hide cursor — consumed and discarded
//
// Any unrecognised escape is consumed defensively so a bug in the
// reporter shows up as missing/extra TEXT, not as escape-code litter
// in the assertion output.
func renderVirtualTerminal(t *testing.T, raw []byte, cols int) []string {
	t.Helper()
	const maxRows = 200
	rows := make([][]rune, 1)
	rows[0] = make([]rune, cols)
	for i := range rows[0] {
		rows[0][i] = ' '
	}
	row, col := 0, 0
	ensureRow := func(r int) {
		for len(rows) <= r {
			nr := make([]rune, cols)
			for i := range nr {
				nr[i] = ' '
			}
			rows = append(rows, nr)
			if len(rows) > maxRows {
				t.Fatalf("virtual terminal grew past %d rows — runaway repaint", maxRows)
			}
		}
	}
	i := 0
	for i < len(raw) {
		b := raw[i]
		switch {
		case b == 0x1b && i+1 < len(raw) && raw[i+1] == '[':
			// CSI: read params then final byte.
			j := i + 2
			for j < len(raw) {
				c := raw[j]
				// Final byte for CSI is in 0x40..0x7E. Params are
				// 0x30..0x3F (digits and ; ? :).
				if c >= 0x40 && c <= 0x7E {
					break
				}
				j++
			}
			if j >= len(raw) {
				return nil
			}
			params := string(raw[i+2 : j])
			final := raw[j]
			switch final {
			case 'A':
				n := 1
				if params != "" {
					_, _ = fmt.Sscanf(params, "%d", &n)
				}
				row -= n
				if row < 0 {
					row = 0
				}
			case 'K':
				// erase line: 2 = whole row, 0/empty = to end, 1 = to start.
				mode := 0
				if params != "" {
					_, _ = fmt.Sscanf(params, "%d", &mode)
				}
				ensureRow(row)
				switch mode {
				case 2:
					for k := range rows[row] {
						rows[row][k] = ' '
					}
				case 0:
					for k := col; k < len(rows[row]); k++ {
						rows[row][k] = ' '
					}
				case 1:
					for k := 0; k <= col && k < len(rows[row]); k++ {
						rows[row][k] = ' '
					}
				}
			case 'm':
				// SGR — colours; ignored.
			case 'h', 'l':
				// cursor visibility (?25h/?25l) — ignored.
			}
			i = j + 1
		case b == '\r':
			col = 0
			i++
		case b == '\n':
			// Real terminals run with ONLCR enabled by default, so a
			// bare '\n' from a Go program also resets the column. We
			// mirror that here so the virtual terminal stays faithful
			// to what the user actually sees.
			row++
			col = 0
			ensureRow(row)
			i++
		default:
			// Decode one rune so wide chars (braille) count as 1 cell.
			r, sz := utf8.DecodeRune(raw[i:])
			if r == utf8.RuneError && sz == 1 {
				i++
				continue
			}
			ensureRow(row)
			if col >= cols {
				// reporter is supposed to never wrap; if it does we
				// emulate the real terminal and start a new row.
				row++
				ensureRow(row)
				col = 0
			}
			rows[row][col] = r
			col++
			i += sz
		}
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = strings.TrimRight(string(r), " ")
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

// TestPrettyReporter_StepLineRepaintsInPlace is the regression for the
// live-stream reporter bug where StepLine calls appended a fresh
// spinner row each time instead of repainting in place. It drives the
// reporter through an EnableStreaming → StepStarted → two StepLine →
// StepFinished cycle, replays the byte stream into a virtual terminal,
// and asserts the final visible frame contains exactly one spinner
// row for the step plus the two tail lines.
func TestPrettyReporter_StepLineRepaintsInPlace(t *testing.T) {
	var buf bytes.Buffer
	r := &PrettyReporter{W: &buf, noColor: true}
	r.EnableStreaming()
	r.StepStarted("foo")
	// Stop the ticker so the test stays deterministic — we only want
	// to observe paints driven by StepLine / StepFinished.
	r.mu.Lock()
	r.stopTickerLocked()
	r.mu.Unlock()
	r.StepLine("foo", "line A", false)
	r.StepLine("foo", "line B", false)

	// Snapshot the running frame before StepFinished erases it. This is
	// the frame the user actually stares at while the step is in flight —
	// the regression manifested exactly here as a stack of duplicate
	// spinner rows.
	runningScreen := renderVirtualTerminal(t, buf.Bytes(), 80)
	runningRows := countSpinnerRowsForStep(runningScreen, "foo")
	if runningRows != 1 {
		t.Errorf("running frame should show exactly 1 spinner row for 'foo', got %d:\n%s",
			runningRows, strings.Join(runningScreen, "\n"))
	}
	joined := strings.Join(runningScreen, "\n")
	if !strings.Contains(joined, "line A") {
		t.Errorf("tail line 'line A' missing from running frame:\n%s", joined)
	}
	if !strings.Contains(joined, "line B") {
		t.Errorf("tail line 'line B' missing from running frame:\n%s", joined)
	}

	r.StepFinished("foo", StatusPassed, time.Second, "")

	raw := buf.Bytes()
	if n := len(raw); n > 1024 {
		t.Fatalf("reporter wrote %d bytes for a trivial scenario — pathological repaint", n)
	}
	finalScreen := renderVirtualTerminal(t, raw, 80)
	if got := countSpinnerRowsForStep(finalScreen, "foo"); got != 0 {
		t.Errorf("final frame still shows %d residual spinner rows for 'foo'; expected 0:\n%s",
			got, strings.Join(finalScreen, "\n"))
	}
	if got := countFrozenRowsForStep(finalScreen, "foo"); got != 1 {
		t.Errorf("final frame should show exactly 1 frozen row for 'foo', got %d:\n%s",
			got, strings.Join(finalScreen, "\n"))
	}
}

// countSpinnerRowsForStep returns the number of rows whose first non-
// blank rune is a spinner-frame glyph followed by the given step name.
// Used to assert the live-block repaint left exactly one in-flight row.
func countSpinnerRowsForStep(screen []string, step string) int {
	n := 0
	for _, ln := range screen {
		trimmed := strings.TrimLeft(ln, " ")
		if trimmed == "" || !strings.Contains(trimmed, step) {
			continue
		}
		first := []rune(trimmed)[0]
		for _, sf := range spinnerFrames {
			if string(first) == sf {
				n++
				break
			}
		}
	}
	return n
}

// countFrozenRowsForStep returns the number of rows whose first non-
// blank rune is the success/fail glyph followed by the given step name.
func countFrozenRowsForStep(screen []string, step string) int {
	n := 0
	for _, ln := range screen {
		trimmed := strings.TrimLeft(ln, " ")
		if trimmed == "" || !strings.Contains(trimmed, step) {
			continue
		}
		first := []rune(trimmed)[0]
		if first == '✓' || first == '✗' {
			n++
		}
	}
	return n
}

// TestPrettyReporter_NoWrapOnNarrowTerminal exercises the same flow
// as the regression test above but on a 60-column virtual terminal
// with tail lines longer than the width. The repaint must still
// resolve to exactly one frozen row for the step at end-of-run: any
// previous spinner repaint that occupied multiple visual rows (via
// terminal auto-wrap) must have been fully erased.
func TestPrettyReporter_NoWrapOnNarrowTerminal(t *testing.T) {
	var buf bytes.Buffer
	r := &PrettyReporter{W: &buf, noColor: true, termWidth: 60}
	r.EnableStreaming()
	r.StepStarted("foo")
	r.mu.Lock()
	r.stopTickerLocked()
	r.mu.Unlock()
	long := strings.Repeat("x", 200)
	r.StepLine("foo", long, false)
	r.StepLine("foo", long, false)
	r.StepLine("foo", long, false)
	r.StepFinished("foo", StatusPassed, time.Second, "")

	screen := renderVirtualTerminal(t, buf.Bytes(), 60)
	spinners := 0
	for _, ln := range screen {
		trimmed := strings.TrimLeft(ln, " ")
		if trimmed == "" {
			continue
		}
		first := []rune(trimmed)[0]
		for _, sf := range spinnerFrames {
			if string(first) == sf && strings.Contains(trimmed, "foo") {
				spinners++
				break
			}
		}
	}
	if spinners != 0 {
		t.Errorf("narrow terminal still shows %d residual spinner rows for 'foo':\n%s",
			spinners, strings.Join(screen, "\n"))
	}
}

// TestEraseLiveLocked_IssuesEraseDownAfterRewind locks the safety
// belt added to fix the duplicated-step-name pile: after walking up
// drawnLines and erasing each row, the function must also issue a
// "clear from cursor to end of screen" so wrapped sub-log lines that
// auto-wrapped on a narrow terminal cannot leave stranded rows that
// the next paint would stack on top of.
func TestEraseLiveLocked_IssuesEraseDownAfterRewind(t *testing.T) {
	var buf bytes.Buffer
	r := &PrettyReporter{W: &buf, noColor: true, live: true}
	r.drawnLines = 3
	r.eraseLiveLocked()
	out := buf.String()
	if !strings.Contains(out, ansiEraseDown) {
		t.Fatalf("eraseLiveLocked must emit ansiEraseDown after the rewind loop, got %q", out)
	}
	// And only when there was actually something to erase.
	buf.Reset()
	r.drawnLines = 0
	r.eraseLiveLocked()
	if strings.Contains(buf.String(), ansiEraseDown) {
		t.Fatalf("eraseLiveLocked must NOT emit ansiEraseDown when drawnLines == 0, got %q", buf.String())
	}
}
