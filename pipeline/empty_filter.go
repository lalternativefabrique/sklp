package pipeline

import (
	"io"
	"strings"
)

// emptyFilter is an io.Writer that drops `EMPTY <pkg>` lines emitted by
// gotestsum for packages with zero tests. They are pure noise during a
// human-watched run; the caller can still surface an aggregated count via
// Suppressed().
type emptyFilter struct {
	w          io.Writer
	buf        []byte
	suppressed int
}

func newEmptyFilter(w io.Writer) *emptyFilter { return &emptyFilter{w: w} }

func (f *emptyFilter) Suppressed() int { return f.suppressed }

func (f *emptyFilter) Write(p []byte) (int, error) {
	f.buf = append(f.buf, p...)
	for {
		i := indexByte(f.buf, '\n')
		if i < 0 {
			break
		}
		line := f.buf[:i+1]
		if !isEmptyPkgLine(line) {
			if _, err := f.w.Write(line); err != nil {
				return 0, err
			}
		} else {
			f.suppressed++
		}
		f.buf = f.buf[i+1:]
	}
	return len(p), nil
}

func (f *emptyFilter) Flush() {
	if len(f.buf) == 0 {
		return
	}
	if !isEmptyPkgLine(f.buf) {
		_, _ = f.w.Write(f.buf)
	} else {
		f.suppressed++
	}
	f.buf = nil
}

// isEmptyPkgLine detects no-test-package noise from both gotestsum and
// `go test ./...`. It catches three variants:
//
//   - gotestsum's bare `EMPTY <pkg>` lines.
//   - the `│ step: EMPTY <pkg>` variant emitted by the pipeline
//     executor's prefix writer.
//   - `go test`'s `?       <pkg>  [no test files]` lines (and the
//     `│ step: ?  <pkg>  [no test files]` prefixed variant) — same
//     signal, different reporter.
func isEmptyPkgLine(p []byte) bool {
	s := strings.TrimRight(string(p), "\n\r")
	s = strings.TrimSpace(s)
	// Strip the "│ <step>: " prefix if any.
	if idx := strings.Index(s, ": "); idx > 0 && strings.HasPrefix(s, "│ ") {
		s = s[idx+2:]
	}
	if idx := strings.Index(s, "│ "); idx == 0 {
		s = strings.TrimSpace(s[2:])
	}
	if strings.HasPrefix(s, "EMPTY ") || s == "EMPTY" {
		return true
	}
	// `go test` "no test files" lines: `?<tab|spaces><pkg><tab|spaces>[no test files]`.
	if strings.HasPrefix(s, "?") && strings.HasSuffix(s, "[no test files]") {
		return true
	}
	return false
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// tailLines keeps only the last `n` lines of `s`. Used to bound the
// stderr_tail surfaced in failure reports and the diagnose prompt.
func tailLines(s string, n int) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
