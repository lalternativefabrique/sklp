package pipeline

import (
	"bytes"
	"io"
	"reflect"
	"testing"
)

// Tail unit tests for stepLineWriter. Covers issue
// d3eba457-04a4-4496-917e-080ea103fdf1: always surface the last 5
// non-empty lines of a step's combined stdout/stderr under its recap
// line, so the operator sees the verdict of DAST scans (ZAP, nuclei),
// shell pipelines, etc. — not just `✓ stepname  26.5s`.

func newTailTestWriter() *stepLineWriter {
	return newStepLineWriter(io.Discard, io.Discard, &stepStats{}, &bytes.Buffer{})
}

// TestStepLineWriter_TailKeepsLastFive: after writing >5 lines the
// ring buffer must surface only the last 5, in observation order.
func TestStepLineWriter_TailKeepsLastFive(t *testing.T) {
	w := newTailTestWriter()
	for _, ln := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		_, _ = w.Write([]byte(ln + "\n"))
	}
	w.Flush()
	got := w.Tail()
	want := []string{"c", "d", "e", "f", "g"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Tail() = %v, want %v", got, want)
	}
}

// TestStepLineWriter_TailIgnoresEmptyAndEmptyPkgLines: blank lines and
// `[no test files]` / EMPTY noise must NOT enter the ring; they would
// dilute the verdict block with non-information.
func TestStepLineWriter_TailIgnoresEmptyAndEmptyPkgLines(t *testing.T) {
	w := newTailTestWriter()
	input := []string{
		"first",
		"",
		"   ",
		"?   github.com/foo/bar  [no test files]",
		"second",
		"",
		"third",
	}
	for _, ln := range input {
		_, _ = w.Write([]byte(ln + "\n"))
	}
	w.Flush()
	got := w.Tail()
	want := []string{"first", "second", "third"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Tail() = %v, want %v", got, want)
	}
}

// TestStepLineWriter_TailIncludesFinalNoNewlineLine: Flush() must
// drain the buffered trailing line so a step that exits without a
// terminating newline (e.g. a `printf "verdict"` last call) still
// contributes its last line to the tail.
func TestStepLineWriter_TailIncludesFinalNoNewlineLine(t *testing.T) {
	w := newTailTestWriter()
	_, _ = w.Write([]byte("line1\nline2\n"))
	_, _ = w.Write([]byte("final-no-newline"))
	w.Flush()
	got := w.Tail()
	want := []string{"line1", "line2", "final-no-newline"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Tail() = %v, want %v", got, want)
	}
}

// TestStepLineWriter_TailReassemblesAcrossWrites: lines split across
// multiple Write() calls must be reassembled before entering the ring,
// otherwise the tail would show fragmented halves of a verdict line.
func TestStepLineWriter_TailReassemblesAcrossWrites(t *testing.T) {
	w := newTailTestWriter()
	_, _ = w.Write([]byte("alpha\nbe"))
	_, _ = w.Write([]byte("ta\ngam"))
	_, _ = w.Write([]byte("ma\n"))
	w.Flush()
	got := w.Tail()
	want := []string{"alpha", "beta", "gamma"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Tail() = %v, want %v", got, want)
	}
}

// TestStepLineWriter_TailEmpty: no input means no tail. The runner
// uses len(Tail()) to decide whether to print anything, so nil here
// is the correct degenerate output.
func TestStepLineWriter_TailEmpty(t *testing.T) {
	w := newTailTestWriter()
	w.Flush()
	if got := w.Tail(); got != nil {
		t.Fatalf("Tail() on empty writer = %v, want nil", got)
	}
}
