package runnerbuild

import "testing"

// A build that fails deep in a long log must still surface its ending: the
// point of the tail is that "exit status 2" alone names no cause.
func TestTailBufferKeepsTheEnd(t *testing.T) {
	tb := &tailBuffer{max: 10}
	for _, chunk := range []string{"aaaaa", "bbbbb", "ccccc"} {
		if _, err := tb.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if got := tb.String(); got != "bbbbbccccc" {
		t.Fatalf("got %q, want the last 10 bytes", got)
	}
}

func TestTailBufferShorterThanMaxIsKeptWhole(t *testing.T) {
	tb := &tailBuffer{max: 100}
	_, _ = tb.Write([]byte("  go: module lookup disabled  \n"))
	if got := tb.String(); got != "go: module lookup disabled" {
		t.Fatalf("got %q", got)
	}
}

func TestTailBufferEmptyStaysEmpty(t *testing.T) {
	tb := &tailBuffer{max: 10}
	if tb.String() != "" {
		t.Fatal("an unwritten buffer must not add noise to the error")
	}
}
