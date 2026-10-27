package reporter

import (
	"os"
)

// isTerminal reports whether f is connected to a real terminal.
//
// We avoid pulling in github.com/mattn/go-isatty as a direct dep — the
// stdlib already exposes everything we need via the file mode: a
// character device is the universal signature of a TTY on both Unix and
// Windows console handles. The check matches what go-isatty itself
// does on platforms where ioctl probing is unavailable.
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}
