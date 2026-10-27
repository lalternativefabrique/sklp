package spacemanager

import (
	"testing"
)

// idxOf returns the position of name in names, or -1.
func idxOf(names []string, name string) int {
	for i, n := range names {
		if n == name {
			return i
		}
	}
	return -1
}

// TestOrder_TopoSortedUnion: DesiredServiceNames returns the union dependency
// graph (services + jobs) topologically sorted — every name appears after each
// of its upstreams, regardless of insertion order.
func TestOrder_TopoSortedUnion(t *testing.T) {
	// Insertion order deliberately scrambled vs. dependency order.
	desired := []Service{
		{Name: "core", DependsOn: []string{"postgres", "nats", "migrate"}},
		{Name: "migrate", Oneshot: true, DependsOn: []string{"postgres"}},
		{Name: "postgres"},
		{Name: "nats"},
	}
	a := NewAgent(newCounting(), nil, desired, testOpts())
	got := a.DesiredServiceNames()

	mustBefore := func(up, down string) {
		t.Helper()
		if idxOf(got, up) >= idxOf(got, down) {
			t.Fatalf("expected %q before %q, got order %v", up, down, got)
		}
	}
	mustBefore("postgres", "migrate")
	mustBefore("postgres", "core")
	mustBefore("nats", "core")
	mustBefore("migrate", "core")
}

// TestOrder_StableForIndependent: independent names keep a deterministic
// (lexicographic) order so launches are reproducible.
func TestOrder_StableForIndependent(t *testing.T) {
	desired := []Service{{Name: "zeta"}, {Name: "alpha"}, {Name: "mid"}}
	a := NewAgent(newCounting(), nil, desired, testOpts())
	got := a.DesiredServiceNames()
	want := []string{"alpha", "mid", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
