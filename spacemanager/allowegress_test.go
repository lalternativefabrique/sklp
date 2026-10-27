package spacemanager

import "testing"

func TestNormalizeAllowEgress(t *testing.T) {
	got, err := normalizeAllowEgress([]string{"1.2.3.4", "10.0.0.0/8", " 8.8.8.8 ", ""})
	if err != nil {
		t.Fatalf("normalizeAllowEgress: %v", err)
	}
	want := []string{"1.2.3.4/32", "10.0.0.0/8", "8.8.8.8/32"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A bad entry must fail the parse rather than be skipped: a space that believes
// it filters a destination it does not is worse than one that refuses to start.
func TestNormalizeAllowEgressRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"not-an-ip", "example.com", "1.2.3.4/99", "::1"} {
		if _, err := normalizeAllowEgress([]string{bad}); err == nil {
			t.Errorf("normalizeAllowEgress(%q) accepted an invalid entry", bad)
		}
	}
}

// An absent allowlist must stay absent: egress is open unless a space opts in,
// so an existing deploy keeps the behaviour it had.
func TestNormalizeAllowEgressEmptyStaysEmpty(t *testing.T) {
	got, err := normalizeAllowEgress(nil)
	if err != nil {
		t.Fatalf("nil allowlist errored: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("nil allowlist yielded %v, want none", got)
	}
}
