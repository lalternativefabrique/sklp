package reporter

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "0.0s"},
		{500 * time.Millisecond, "0.5s"},
		{28*time.Second + 400*time.Millisecond, "28.4s"},
		{84 * time.Second, "1m24s"},
		{2*time.Minute + 5*time.Second, "2m05s"},
	}
	for _, c := range cases {
		got := FormatDuration(c.in)
		if got != c.want {
			t.Errorf("FormatDuration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNew_NoColorForcesPlain(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var buf bytes.Buffer
	r := New(&buf, Options{})
	if _, ok := r.(*PlainReporter); !ok {
		t.Fatalf("NO_COLOR did not select PlainReporter, got %T", r)
	}
}

func TestNew_NonTTYSelectsPlain(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf, Options{})
	if _, ok := r.(*PlainReporter); !ok {
		t.Fatalf("non-*os.File writer should select PlainReporter, got %T", r)
	}
}

func TestNew_ForcePlain(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf, Options{ForcePlain: true})
	if _, ok := r.(*PlainReporter); !ok {
		t.Fatalf("ForcePlain ignored, got %T", r)
	}
}

func TestPlainReporter_SuccessLayout(t *testing.T) {
	var buf bytes.Buffer
	r := &PlainReporter{W: &buf}
	r.Header("sklp ci · 3 files changed")
	r.StepStarted("tools")
	r.StepFinished("tools", StatusPassed, 400*time.Millisecond, "")
	r.StepStarted("go-test")
	r.StepFinished("go-test", StatusPassed, 24100*time.Millisecond, "180 passed")
	r.Finalize("CI", StatusPassed, 28400*time.Millisecond, []FinalField{
		{Label: "go-test", Value: "180 passed"},
	}, nil)

	out := buf.String()
	for _, want := range []string{
		"sklp ci",
		"tools: started",
		"tools: passed (0.4s)",
		"go-test: passed (24.1s) — 180 passed",
		"CI PASSED (28.4s)",
		"go-test: 180 passed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plain output missing %q\n--- got:\n%s", want, out)
		}
	}
}

func TestPlainReporter_FailureLayout(t *testing.T) {
	var buf bytes.Buffer
	r := &PlainReporter{W: &buf}
	r.StepFinished("go-test", StatusFailed, 14900*time.Millisecond, "2 failed · 178 passed")
	r.StepFinished("lint", StatusSkipped, 0, "")
	r.Failures("FAILURES", []Failure{
		{
			Scope:    "billing",
			Title:    "TestCreateCheckout",
			Message:  "expected status 200, got 500\ninternal: db refused",
			Location: "checkout_test.go:142",
		},
	})
	r.Finalize("CI", StatusFailed, 18700*time.Millisecond, nil, nil)

	out := buf.String()
	for _, want := range []string{
		"go-test: failed (14.9s) — 2 failed",
		"lint: skipped",
		"FAILURES",
		"billing > TestCreateCheckout",
		"expected status 200, got 500",
		"checkout_test.go:142",
		"CI FAILED (18.7s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plain failure output missing %q\n--- got:\n%s", want, out)
		}
	}
}

func TestPrettyReporter_NoColorMode(t *testing.T) {
	var buf bytes.Buffer
	r := &PrettyReporter{W: &buf, noColor: true}
	r.StepFinished("tools", StatusPassed, 400*time.Millisecond, "")
	out := buf.String()
	if strings.Contains(out, "\033[") {
		t.Errorf("noColor reporter emitted ANSI codes: %q", out)
	}
	if !strings.Contains(out, "✓") {
		t.Errorf("expected check glyph, got: %q", out)
	}
	if !strings.Contains(out, "tools") {
		t.Errorf("expected step name, got: %q", out)
	}
	if !strings.Contains(out, "0.4s") {
		t.Errorf("expected duration, got: %q", out)
	}
}

func TestPrettyReporter_ASCIIMode(t *testing.T) {
	var buf bytes.Buffer
	r := &PrettyReporter{W: &buf, ascii: true, noColor: true}
	r.StepFinished("lint", StatusFailed, 1100*time.Millisecond, "")
	r.Finalize("CI", StatusFailed, 1100*time.Millisecond, nil, nil)
	out := buf.String()
	if strings.ContainsAny(out, "✓✗⊘━─") {
		t.Errorf("ascii mode leaked UTF-8 glyph: %q", out)
	}
	if !strings.Contains(out, "[X]") {
		t.Errorf("expected [X] glyph in ascii mode, got: %q", out)
	}
}

func TestPrettyReporter_SuccessBanner(t *testing.T) {
	var buf bytes.Buffer
	r := &PrettyReporter{W: &buf, noColor: true}
	r.Finalize("CI", StatusPassed, 28400*time.Millisecond, []FinalField{
		{Label: "tools", Value: "pnpm 10.18.1 · go 1.25.0"},
		{Label: "go-test", Value: "180 passed"},
	}, nil)
	out := buf.String()
	for _, want := range []string{
		"━",
		"CI PASSED",
		"28.4s",
		"tools",
		"pnpm 10.18.1",
		"go-test",
		"180 passed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("pretty success banner missing %q\n--- got:\n%s", want, out)
		}
	}
}
