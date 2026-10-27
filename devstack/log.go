package devstack

import (
	"fmt"
	"io"
	"os"
	"sync"
)

// logPrinter writes service logs to stdout with a fixed-width prefix.
// All Print calls go through one mutex so lines from concurrent
// services don't interleave mid-line. Width is the longest service
// name so prefixes align visually.
type logPrinter struct {
	mu    sync.Mutex
	out   io.Writer
	width int
	color bool
	pal   map[string]string
}

func newLogPrinter(out io.Writer, names []string, color bool) *logPrinter {
	w := 0
	for _, n := range names {
		if len(n) > w {
			w = len(n)
		}
	}
	pal := map[string]string{}
	if color {
		// Stable color per service so the same name always gets the
		// same hue across runs — easier on the eyes when diffing logs.
		palette := []string{"\x1b[36m", "\x1b[33m", "\x1b[32m", "\x1b[35m", "\x1b[34m", "\x1b[31m"}
		for i, n := range names {
			pal[n] = palette[i%len(palette)]
		}
	}
	return &logPrinter{out: out, width: w, color: color, pal: pal}
}

// Print writes one prefixed line. The trailing newline is added here
// — callers pass the raw scanned line.
func (p *logPrinter) Print(name, line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	prefix := fmt.Sprintf("[%-*s]", p.width, name)
	if p.color {
		c := p.pal[name]
		fmt.Fprintf(p.out, "%s%s\x1b[0m %s\n", c, prefix, line)
		return
	}
	fmt.Fprintf(p.out, "%s %s\n", prefix, line)
}

// Banner writes a supervisor-level message (not a service log) using
// the same writer so output ordering is preserved.
func (p *logPrinter) Banner(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintf(p.out, format+"\n", args...)
}

// stdoutColorEnabled reports whether stdout looks like a TTY. We
// don't pull in mattn/go-isatty for a one-liner — a Stat on stdout is
// portable enough.
func stdoutColorEnabled() bool {
	st, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}
