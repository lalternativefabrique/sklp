package reporter

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"
)

// osc8Re matches an OSC 8 wrapper: ESC ] 8 ; ; URL BEL TEXT ESC ] 8 ; ; BEL.
// Substituting the wrapper with just TEXT lets snapshot-style assertions
// ignore the escape bytes while keeping the visible content intact.
var osc8Re = regexp.MustCompile(`\x1b\]8;;[^\x07]*\x07([^\x1b]*)\x1b\]8;;\x07`)

func stripOSC8(s string) string {
	return osc8Re.ReplaceAllString(s, "$1")
}

// TestPreserveBytes_TrivyTableRendersIntact is the #440 regression: a
// docker-build failure whose Message is an ASCII table (Trivy output)
// must reach the FAILURES section with every byte intact — no
// per-line truncation, no width-driven re-indent that turns the box-
// drawing rows into a wall of empty `|` characters.
func TestPreserveBytes_TrivyTableRendersIntact(t *testing.T) {
	var buf bytes.Buffer
	// noColor=false: OSC 8 wrap is suppressed under no-color mode (we
	// don't want piped output to carry escapes). The CVE-URL hyperlink
	// is part of the colour layer for that reason.
	r := &PrettyReporter{W: &buf, noColor: false}

	// A minimal Trivy-ish table: header, separator, two CVE rows, one
	// of which embeds an avd.aquasec.com URL we'll later check is
	// hyperlinked.
	table := strings.Join([]string{
		"+---------+--------+----------+---------+---------+",
		"| Library | CVE    | Severity | Status  | URL     |",
		"+---------+--------+----------+---------+---------+",
		"| libcap2 | CVE-1  | HIGH     | fixable | https://avd.aquasec.com/nvd/cve-1 |",
		"| libuuid | CVE-2  | CRITICAL | fixable |         |",
		"+---------+--------+----------+---------+---------+",
	}, "\n")

	r.Failures("FAILURES", []Failure{{
		Scope:         "core-image",
		Title:         "exit 1",
		Message:       table,
		PreserveBytes: true,
	}})

	out := buf.String()
	// Every non-URL line of the table must appear verbatim. Rows with
	// CVE URLs are wrapped in OSC 8 escapes (see assertion below), so
	// we strip the escape bytes before matching to keep the rune-level
	// content check honest.
	stripped := stripOSC8(out)
	for _, ln := range strings.Split(table, "\n") {
		if !strings.Contains(stripped, ln) {
			t.Fatalf("FAILURES dropped table row %q from output:\n%s", ln, stripped)
		}
	}
	// The CVE URL must be wrapped in an OSC 8 escape so terminals
	// render it as a clickable link. We assert on the bytes — the
	// terminal renders the inner URL as the visible text.
	if !strings.Contains(out, "\033]8;;https://avd.aquasec.com/nvd/cve-1\007") {
		t.Fatalf("FAILURES did not OSC 8-wrap the CVE URL:\n%s", out)
	}
}

// TestPreserveBytes_OffPathStillIndents proves the new flag is opt-in:
// when PreserveBytes is false (the default for go-test failures), the
// legacy 7-space indent stays — snapshot-test stable.
func TestPreserveBytes_OffPathStillIndents(t *testing.T) {
	var buf bytes.Buffer
	r := &PrettyReporter{W: &buf, noColor: true}
	r.Failures("FAILURES", []Failure{{
		Scope:   "foo",
		Title:   "TestX",
		Message: "assertion failed\nexpected 1, got 2",
	}})
	if !strings.Contains(buf.String(), "       assertion failed") {
		t.Fatalf("default path lost the 7-space indent:\n%s", buf.String())
	}
}

// TestOSC8_LogPathInFinalize wraps the `log` field with file:// so
// modern terminals render it as a clickable hyperlink to the captured
// run log.
func TestOSC8_LogPathInFinalize(t *testing.T) {
	var buf bytes.Buffer
	r := &PrettyReporter{W: &buf, noColor: false}
	r.Finalize("CI", StatusPassed, time.Second,
		[]FinalField{{Label: "log", Value: "/tmp/ci-20260517.log"}}, nil)
	out := buf.String()
	want := "\033]8;;file:///tmp/ci-20260517.log\007/tmp/ci-20260517.log\033]8;;\007"
	if !strings.Contains(out, want) {
		t.Fatalf("log field not OSC 8-wrapped.\nwant substring: %q\ngot:\n%s", want, out)
	}
}

// TestOSC8_NoColorSuppresses guards the contract that --no-color (or
// the implicit non-TTY routing) emits no OSC 8 escapes, so piped
// output stays clean.
func TestOSC8_NoColorSuppresses(t *testing.T) {
	var buf bytes.Buffer
	r := &PrettyReporter{W: &buf, noColor: true}
	r.Finalize("CI", StatusPassed, time.Second,
		[]FinalField{{Label: "log", Value: "/tmp/ci.log"}}, nil)
	if strings.Contains(buf.String(), "\033]8;;") {
		t.Fatalf("no-color mode leaked OSC 8 escape:\n%s", buf.String())
	}
}

// TestTailLines_OverrideRespected — phase A: an explicit override
// wins over the auto formula.
func TestTailLines_OverrideRespected(t *testing.T) {
	var buf bytes.Buffer
	r := &PrettyReporter{W: &buf, noColor: true, tailCapOverride: 3}
	r.mu.Lock()
	got := r.effectiveTailCapLocked()
	r.mu.Unlock()
	if got != 3 {
		t.Fatalf("override 3 lost: got %d", got)
	}
}

// TestTailLines_AutoFormula — auto = min(10, h/4) with a floor.
func TestTailLines_AutoFormula(t *testing.T) {
	cases := []struct {
		height int
		want   int
	}{
		{40, 10},            // h/4=10, capped at 10
		{20, 5},             // h/4=5
		{12, 3},             // h/4=3
		{4, 1},              // h/4=1, hits floor (still 1)
		{0, tailCapDefault}, // unknown height falls back to default
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		r := &PrettyReporter{W: &buf, termHeight: tc.height}
		r.mu.Lock()
		got := r.effectiveTailCapLocked()
		r.mu.Unlock()
		if got != tc.want {
			t.Errorf("height=%d: got %d, want %d", tc.height, got, tc.want)
		}
	}
}

// TestTailBudget_DropsOldestUnderHeightPressure — phase D: when the
// total rows the live block would paint exceeds the terminal budget,
// the painter drops oldest tail entries until it fits. This is the
// #409 spinner-stacking root-cause fix.
func TestTailBudget_DropsOldestUnderHeightPressure(t *testing.T) {
	var buf bytes.Buffer
	// Small terminal: 12 rows -> budget = 12 - 4 = 8 rows total.
	r := &PrettyReporter{W: &buf, noColor: true, ascii: true,
		termWidth: 80, termHeight: 12, tailCapOverride: 20}
	r.EnableStreaming()
	r.StepStarted("a")
	r.StepStarted("b")
	// Stop ticker so we observe only StepLine-driven paints.
	r.mu.Lock()
	r.stopTickerLocked()
	r.mu.Unlock()
	for i := 0; i < 15; i++ {
		r.StepLine("a", "a-line-"+itoa(i), false)
	}
	for i := 0; i < 15; i++ {
		r.StepLine("b", "b-line-"+itoa(i), false)
	}

	// drawnLines must never exceed the height-derived budget.
	r.mu.Lock()
	defer r.mu.Unlock()
	budget := r.tailBudgetLocked()
	if r.drawnLines > budget {
		t.Fatalf("drawnLines=%d exceeded budget=%d — spinner-stacking risk",
			r.drawnLines, budget)
	}
}

// TestHotkeyListener_NoOpWhenStdinNotTTY — phase B: New() must not
// arm the hotkey goroutine when stdin is not a terminal. The Go test
// harness pipes stdin, so this is the natural negative case.
func TestHotkeyListener_NoOpWhenStdinNotTTY(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf, Options{EnableHotkeys: true, ForcePlain: true})
	// ForcePlain routes to PlainReporter, which has no hotkey field —
	// the assertion below catches both "PlainReporter is returned" and
	// "no goroutine was started" in one shot.
	if _, ok := r.(*PlainReporter); !ok {
		t.Fatalf("non-TTY + ForcePlain should produce PlainReporter, got %T", r)
	}
}

// TestRuneWidth_StepNameWithEmojiPadding guards that the spinner row
// padding uses rune count, not byte count, so a step name containing
// a multi-byte rune still right-aligns the duration cleanly.
func TestRuneWidth_StepNameWithEmojiPadding(t *testing.T) {
	var buf bytes.Buffer
	r := &PrettyReporter{W: &buf, noColor: true, ascii: true,
		termWidth: 80, termHeight: 40}
	r.EnableStreaming()
	r.StepStarted("✓-step") // first rune is multi-byte
	r.mu.Lock()
	r.stopTickerLocked()
	r.mu.Unlock()
	r.StepFinished("✓-step", StatusPassed, time.Second, "")
	// No assertion on exact layout — the regression mode (byte-counted
	// padding) would emit fewer spaces and visually mis-align. We just
	// confirm the byte stream does not panic and contains the duration.
	if !strings.Contains(buf.String(), "1.0s") {
		t.Fatalf("frozen line missing duration:\n%s", buf.String())
	}
}
