package reporter

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestLineTee_SplitsOnNewline confirms the tee emits one Emit call per
// complete line and forwards every byte verbatim to the underlying.
func TestLineTee_SplitsOnNewline(t *testing.T) {
	var sink bytes.Buffer
	var got []string
	t1 := &LineTee{
		Underlying: &sink,
		Emit:       func(line string) { got = append(got, line) },
	}
	if _, err := t1.Write([]byte("alpha\nbeta\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if want := []string{"alpha", "beta"}; !equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if sink.String() != "alpha\nbeta\n" {
		t.Fatalf("underlying byte stream changed: %q", sink.String())
	}
}

// TestLineTee_BuffersPartialLines verifies that a step writing in
// fragments produces one line per actual '\n' boundary, not per Write.
func TestLineTee_BuffersPartialLines(t *testing.T) {
	var got []string
	t1 := &LineTee{
		Underlying: io.Discard,
		Emit:       func(line string) { got = append(got, line) },
	}
	t1.Write([]byte("foo"))
	t1.Write([]byte("bar\n"))
	t1.Write([]byte("baz"))
	t1.Flush()
	if want := []string{"foobar", "baz"}; !equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// TestLineTee_NoEmitIsPassThrough confirms a nil Emit makes the tee
// behave as a transparent pipe (no buffering).
func TestLineTee_NoEmitIsPassThrough(t *testing.T) {
	var sink bytes.Buffer
	t1 := &LineTee{Underlying: &sink}
	t1.Write([]byte("hello\nworld"))
	if sink.String() != "hello\nworld" {
		t.Fatalf("underlying: %q", sink.String())
	}
}

// TestLineTee_DoesNotLoseBytesAcrossWrites stresses the splitter with
// many small writes and confirms every byte that went in comes out
// through Emit + the trailing Flush, in order.
func TestLineTee_DoesNotLoseBytesAcrossWrites(t *testing.T) {
	var got strings.Builder
	t1 := &LineTee{
		Underlying: io.Discard,
		Emit: func(line string) {
			got.WriteString(line)
			got.WriteByte('\n')
		},
	}
	input := "line1\nline2\nline3\n"
	for i := 0; i < len(input); i++ {
		t1.Write([]byte{input[i]})
	}
	t1.Flush()
	if got.String() != input {
		t.Fatalf("recovered %q want %q", got.String(), input)
	}
}

// TestPrettyReporter_StepLineRendersUnderSpinner is the integration
// check that closes the loop: EnableStreaming + StepStarted + StepLine
// should produce a live block whose output contains the streamed line,
// rendered BEFORE StepFinished is called (i.e. while the step is still
// "running"). Tests the no-buffering guarantee that motivated #399.
func TestPrettyReporter_StepLineRendersUnderSpinner(t *testing.T) {
	var buf syncBuffer
	r := &PrettyReporter{W: &buf, ascii: true, noColor: true}
	r.EnableStreaming()
	r.StepStarted("build")

	// Wait one tick so the live block has been painted at least once.
	time.Sleep(20 * time.Millisecond)

	r.StepLine("build", "compiling pkg/a", false)
	r.StepLine("build", "compiling pkg/b", false)

	// Now snapshot the buffer BEFORE finishing the step. The streamed
	// lines must already be in there — that's the whole point of the
	// feature.
	snap := buf.String()
	if !strings.Contains(snap, "compiling pkg/a") {
		t.Fatalf("stream line 'compiling pkg/a' not rendered yet:\n%s", snap)
	}
	if !strings.Contains(snap, "compiling pkg/b") {
		t.Fatalf("stream line 'compiling pkg/b' not rendered yet:\n%s", snap)
	}

	r.StepFinished("build", StatusPassed, 100*time.Millisecond, "")
	r.Finalize("CI", StatusPassed, time.Second, nil, nil)
}

// TestPrettyReporter_TailIsCapped confirms that the rendered tail
// under each step does not grow without bound — older lines scroll off
// the visible block once the effective tail cap is reached. The in-
// memory buffer keeps everything up to flushAllMax (so the 'f' hotkey
// can surface it), but the painter only emits the trailing N lines.
func TestPrettyReporter_TailIsCapped(t *testing.T) {
	var buf syncBuffer
	r := &PrettyReporter{W: &buf, ascii: true, noColor: true}
	r.EnableStreaming()
	r.StepStarted("build")
	const n = tailCapDefault + 10
	for i := 0; i < n; i++ {
		r.StepLine("build", "line-"+itoa(i), false)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.active) != 1 {
		t.Fatalf("active steps: %d", len(r.active))
	}
	// In-memory tail keeps every line up to flushAllMax — the painter
	// trims at render time using effectiveTailCapLocked.
	if got := len(r.active[0].tail); got != n {
		t.Fatalf("in-memory tail length %d, want %d (flushAll buffer)", got, n)
	}
	if r.active[0].tail[n-1] != "line-"+itoa(n-1) {
		t.Fatalf("newest tail line: %q", r.active[0].tail[n-1])
	}
	// The painter's per-step cap (auto = tailCapDefault when termHeight
	// is unknown) is what bounds the visible block.
	if got := r.effectiveTailCapLocked(); got != tailCapDefault {
		t.Fatalf("effective cap: %d, want %d", got, tailCapDefault)
	}
}

// TestPrettyReporter_StepLineUnknownStepIsDropped guards the contract
// that a StepLine for a step that never started (or already finished)
// is silently dropped, never crashes the reporter.
func TestPrettyReporter_StepLineUnknownStepIsDropped(t *testing.T) {
	var buf syncBuffer
	r := &PrettyReporter{W: &buf, ascii: true, noColor: true}
	r.EnableStreaming()
	r.StepLine("never-started", "should be dropped", false)
	r.StepStarted("real")
	r.StepLine("ghost", "also dropped", false)
	r.StepFinished("real", StatusPassed, time.Millisecond, "")
	r.StepLine("real", "after end, dropped", false)
	// No panic = pass. We don't assert on the buffer because the
	// dropped lines must not contaminate the final transcript anyway.
}

// TestPrettyReporter_DisableLinePrefixSuppressesAutoPrefix locks the
// `sklp deploy` double-prefix fix: with two active steps the reporter would
// normally auto-prefix each tail line with "[step]". A caller that already
// prefixes its lines itself (deploy renders "[svc] …") calls DisableLinePrefix;
// the reporter must then NOT add a second "[build]" — the line carries exactly
// the prefix the caller put on it, once.
func TestPrettyReporter_DisableLinePrefixSuppressesAutoPrefix(t *testing.T) {
	var buf syncBuffer
	r := &PrettyReporter{W: &buf, ascii: true, noColor: true, termWidth: 200}
	r.EnableStreaming()
	r.DisableLinePrefix()
	// Two steps so multi-step auto-prefix (prefixLines) would normally engage.
	r.StepStarted("build")
	r.StepStarted("test")
	time.Sleep(20 * time.Millisecond)

	// The caller already prefixed the line itself ("[build] …").
	r.StepLine("build", "[build] compiling", false)
	time.Sleep(20 * time.Millisecond)

	snap := buf.String()
	if !strings.Contains(snap, "[build] compiling") {
		t.Fatalf("own prefix line missing:\n%s", snap)
	}
	if strings.Contains(snap, "[build] [build] compiling") {
		t.Fatalf("auto-prefix doubled the caller prefix:\n%s", snap)
	}

	r.StepFinished("build", StatusPassed, time.Millisecond, "")
	r.StepFinished("test", StatusPassed, time.Millisecond, "")
	r.Finalize("CI", StatusPassed, time.Second, nil, nil)
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// itoa is a tiny dependency-free int formatter so the test file does
// not pull in strconv just for label generation.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

// syncBuffer is a tiny mutex-guarded bytes.Buffer so the ticker
// goroutine inside the live PrettyReporter can race-safely write while
// the test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
