package pipeline

import (
	"bytes"
	"strings"
	"testing"
)

// TestEmptyFilter_SuppressesGotestsumEmptyLines confirms the writer
// drops `EMPTY <pkg>` noise — including the version that comes through
// the pipeline executor's `│ step: ` prefix — while leaving every other
// line untouched.
func TestEmptyFilter_SuppressesGotestsumEmptyLines(t *testing.T) {
	var buf bytes.Buffer
	f := newEmptyFilter(&buf)

	in := strings.Join([]string{
		"PASS account",
		"EMPTY .",
		"│ go-test: EMPTY account",
		"│ go-test: PASS billing/checkout",
		"EMPTY apps/llm",
		"?       github.com/example/agent  [no test files]",
		"│ go-test: ?       github.com/example/cmd   [no test files]",
		"some other line",
		"",
	}, "\n")

	if _, err := f.Write([]byte(in)); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Flush()

	out := buf.String()
	for _, want := range []string{
		"PASS account",
		"│ go-test: PASS billing/checkout",
		"some other line",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, banned := range []string{"EMPTY .", "EMPTY account", "EMPTY apps/llm", "no test files"} {
		if strings.Contains(out, banned) {
			t.Errorf("unexpected %q in:\n%s", banned, out)
		}
	}
	if got, want := f.Suppressed(), 5; got != want {
		t.Errorf("Suppressed() = %d, want %d", got, want)
	}
}

// TestEmptyFilter_PreservesPartialLines exercises the buffering: a
// caller streaming a half line and then completing it must see the same
// output as a one-shot write.
func TestEmptyFilter_PreservesPartialLines(t *testing.T) {
	var buf bytes.Buffer
	f := newEmptyFilter(&buf)
	if _, err := f.Write([]byte("hel")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("lo\n")); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "hello\n" {
		t.Errorf("got %q, want %q", got, "hello\n")
	}
}
