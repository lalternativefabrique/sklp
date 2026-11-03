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
