package reporter

import (
	"io"
	"os"
	"strconv"
)

// RelayedTTYEnv is set to "1" by a parent sklp that copies this process's
// stdout/stderr verbatim onto its own terminal (the in-space runner behind
// `sklp run`, the vz guest behind the macOS relay).
const RelayedTTYEnv = "SKLP_TTY"

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

// IsTerminal reports whether w ends on a terminal, either directly or
// through a parent sklp that relays this process's stdout/stderr to one.
func IsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	if isTerminal(f) {
		return true
	}
	return os.Getenv(RelayedTTYEnv) == "1" && (f == os.Stdout || f == os.Stderr)
}

func relayedSize() (cols, rows int) {
	cols, _ = strconv.Atoi(os.Getenv("COLUMNS"))
	rows, _ = strconv.Atoi(os.Getenv("LINES"))
	return cols, rows
}
