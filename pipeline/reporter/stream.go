package reporter

import (
	"bytes"
	"io"
	"sync"
)

// LineTee wraps an underlying io.Writer and, in addition to forwarding
// every byte verbatim, splits the stream on '\n' boundaries and calls
// `emit` once per complete line. Partial trailing bytes are buffered
// across calls so a step that writes `"foo"` then `"bar\n"` produces a
// single emitted line `"foobar"`.
//
// LineTee is the bridge between the per-step writer chain (which feeds
// the log file + filter writer) and the StepStreamer reporter
// capability: the runner wraps each step's stdout/stderr writer in a
// LineTee so live lines reach the reporter at the same time the bytes
// reach disk.
//
// Splitting is dumb on purpose: '\n' only, no carriage-return handling,
// no UTF-8 validation, no length cap. The cost is one allocation per
// line. Anything fancier (colour stripping, regex matching) belongs in
// a layer above, not here.
type LineTee struct {
	// Underlying is the writer that receives every byte verbatim. It is
	// the authoritative sink for the step's output (e.g. the per-run
	// log file + filter chain).
	Underlying io.Writer

	// Emit is called once per complete line. Newline characters are
	// stripped from `line`. May be nil; the LineTee then degrades to a
	// pass-through over Underlying.
	Emit func(line string)

	mu  sync.Mutex
	buf bytes.Buffer
}

// Write forwards p to Underlying and, between writes, slices off any
// completed lines and hands them to Emit. The return value is whatever
// Underlying returns — the streaming side never short-circuits the
// underlying write, so a slow / failing Emit can't corrupt the log
// capture.
func (t *LineTee) Write(p []byte) (int, error) {
	n, err := t.Underlying.Write(p)
	if t.Emit == nil {
		return n, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Write(p)
	for {
		raw := t.buf.Bytes()
		i := bytes.IndexByte(raw, '\n')
		if i < 0 {
			break
		}
		line := string(raw[:i])
		t.buf.Next(i + 1)
		t.Emit(line)
	}
	return n, err
}

// Flush drains any buffered trailing bytes (a final line without a
// terminating newline) and emits them. Safe to call multiple times.
func (t *LineTee) Flush() {
	if t.Emit == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.buf.Len() == 0 {
		return
	}
	t.Emit(t.buf.String())
	t.buf.Reset()
}
