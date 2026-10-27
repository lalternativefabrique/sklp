package spacemanager

import (
	"testing"
	"time"
)

func TestBackoffCappedExponential(t *testing.T) {
	b := newBackoff(1*time.Second, 30*time.Second)
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30}
	for i, w := range want {
		got := b.next()
		if got != w*time.Second {
			t.Fatalf("step %d: got %v want %v", i, got, w*time.Second)
		}
	}
}

func TestBackoffReset(t *testing.T) {
	b := newBackoff(1*time.Second, 30*time.Second)
	b.next()
	b.next() // cur now 4s
	b.reset()
	if got := b.next(); got != 1*time.Second {
		t.Fatalf("after reset got %v want 1s", got)
	}
}
