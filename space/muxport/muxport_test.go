package muxport

import "testing"

func TestForIsStableAndInRange(t *testing.T) {
	if For("") != Base {
		t.Fatalf("empty scope = %d", For(""))
	}
	p := For("sklp-dev-f73d731bad0b")
	if p != For("sklp-dev-f73d731bad0b") || p < Base || p >= Base+Span {
		t.Fatalf("For = %d", p)
	}
}

func TestFollowingCoversTheGuestWalk(t *testing.T) {
	base := For("sklp-dev-5159c4a7691f")
	got := Following("sklp-dev-5159c4a7691f")
	if len(got) != Spare {
		t.Fatalf("got %d ports, want %d", len(got), Spare)
	}
	for i, p := range got {
		if p != base+i+1 {
			t.Fatalf("Following[%d] = %d, want %d", i, p, base+i+1)
		}
	}
}
