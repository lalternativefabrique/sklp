package pipeline

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/pipeline/reporter"
)

func captureStderrFile(t *testing.T, fn func(stderr *os.File)) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stderr
	os.Stderr = w
	fn(w)
	os.Stderr = prev
	w.Close()
	out, _ := io.ReadAll(r)
	r.Close()
	return string(out)
}

func TestEmitJSONReportMirrorsSentinelOnPipe(t *testing.T) {
	t.Setenv(reporter.RelayedTTYEnv, "")
	var stdout bytes.Buffer
	got := captureStderrFile(t, func(stderr *os.File) { emitJSONReport(&stdout, stderr, []byte(`{"pass_fail":"pass"}`)) })
	if !strings.Contains(got, ReportSentinelBegin) {
		t.Fatalf("pipe consumers need the sentinel block, got %q", got)
	}
}

func TestEmitJSONReportKeepsSentinelOffATerminal(t *testing.T) {
	t.Setenv(reporter.RelayedTTYEnv, "1")
	var stdout bytes.Buffer
	got := captureStderrFile(t, func(stderr *os.File) { emitJSONReport(&stdout, stderr, []byte(`{"pass_fail":"pass"}`)) })
	if got != "" {
		t.Fatalf("a terminal must not see the machine report, got %q", got)
	}
	if !strings.Contains(stdout.String(), "pass_fail") {
		t.Fatal("stdout still carries the report")
	}
}
