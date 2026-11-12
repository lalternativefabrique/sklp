package reporter

import (
	"bytes"
	"os"
	"testing"
)

func swapStdout(t *testing.T) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stdout
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = prev
		w.Close()
		r.Close()
	})
	return w
}

func TestIsTerminalPipeIsNotATerminal(t *testing.T) {
	t.Setenv(RelayedTTYEnv, "")
	if IsTerminal(swapStdout(t)) {
		t.Fatal("a pipe on stdout must not count as a terminal")
	}
}

func TestIsTerminalHonoursRelayedTTYOnStdio(t *testing.T) {
	t.Setenv(RelayedTTYEnv, "1")
	if !IsTerminal(swapStdout(t)) {
		t.Fatal("stdout relayed to a terminal by a parent sklp must count as one")
	}
}

func TestIsTerminalRelayedTTYIgnoresOtherFiles(t *testing.T) {
	t.Setenv(RelayedTTYEnv, "1")
	f, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if IsTerminal(f) {
		t.Fatal("a log file is never a terminal, relayed or not")
	}
	if IsTerminal(&bytes.Buffer{}) {
		t.Fatal("a buffer is never a terminal")
	}
}

func TestNewPicksPrettyForRelayedTTY(t *testing.T) {
	t.Setenv(RelayedTTYEnv, "1")
	t.Setenv("NO_COLOR", "")
	t.Setenv("COLUMNS", "132")
	t.Setenv("LINES", "40")
	rep, ok := New(swapStdout(t), Options{}).(*PrettyReporter)
	if !ok {
		t.Fatal("want the pretty reporter on a relayed terminal")
	}
	if got := rep.currentWidth(); got != 132 {
		t.Fatalf("width = %d, want COLUMNS=132", got)
	}
	if got := rep.currentHeight(); got != 40 {
		t.Fatalf("height = %d, want LINES=40", got)
	}
}
