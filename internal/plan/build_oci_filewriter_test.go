//go:build linux

package plan

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestFileWriterDrainsIntoNonFileWriter(t *testing.T) {
	var buf bytes.Buffer
	f, wait, err := fileWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("trivy report line\n", 20000)
	if _, err := f.WriteString(payload); err != nil {
		t.Fatal(err)
	}
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	if buf.String() != payload {
		t.Fatalf("got %d bytes, want %d", buf.Len(), len(payload))
	}
}

func TestFileWriterPassesThroughFile(t *testing.T) {
	f, wait, err := fileWriter(os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	if f != os.Stderr {
		t.Fatal("expected passthrough of *os.File")
	}
	if err := wait(); err != nil {
		t.Fatal(err)
	}
}

func TestWithFileWritersFlushesBeforeReturn(t *testing.T) {
	var out, errOut bytes.Buffer
	err := withFileWriters(&out, &errOut, func(o, e *os.File) error {
		o.WriteString("scan stdout")
		e.WriteString("scan stderr")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "scan stdout" || errOut.String() != "scan stderr" {
		t.Fatalf("out=%q err=%q", out.String(), errOut.String())
	}
}
