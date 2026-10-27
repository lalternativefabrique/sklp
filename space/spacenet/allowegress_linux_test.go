//go:build linux

package spacenet

import (
	"strings"
	"testing"
)

// No allowlist must render no rules at all: egress stays open, so an existing
// space is untouched by the feature's mere existence.
func TestAllowEgressRules_EmptyRendersNothing(t *testing.T) {
	if got := allowEgressRules("10.88.0.0/24", "tap0", nil); got != "" {
		t.Errorf("an empty allowlist rendered rules:\n%s", got)
	}
}

// The drop must come last, and every allowed destination before it: nft
// evaluates in order, so a drop placed first would deny what follows it.
func TestAllowEgressRules_DropIsLast(t *testing.T) {
	rules := allowEgressRules("10.88.0.0/24", "tap0", []string{"1.2.3.4/32", "8.8.8.8/32"})
	lines := strings.Split(strings.TrimSpace(rules), "\n")

	last := lines[len(lines)-1]
	if !strings.Contains(last, "drop") {
		t.Fatalf("last rule is %q, want the default-deny drop", last)
	}
	for i, l := range lines[:len(lines)-1] {
		if strings.Contains(l, " drop") {
			t.Errorf("line %d drops before the allowlist is evaluated: %q", i, l)
		}
	}
	for _, cidr := range []string{"1.2.3.4/32", "8.8.8.8/32"} {
		if !strings.Contains(rules, cidr) {
			t.Errorf("allowed destination %s is missing from the ruleset", cidr)
		}
	}
}

// DNS must survive the allowlist: the space resolver forwards upstream through
// the tap, and denying that would break name resolution for the very
// destinations the allowlist permits.
func TestAllowEgressRules_DNSStaysReachable(t *testing.T) {
	rules := allowEgressRules("10.88.0.0/24", "tap0", []string{"1.2.3.4/32"})
	for _, want := range []string{"udp dport 53 accept", "tcp dport 53 accept"} {
		if !strings.Contains(rules, want) {
			t.Errorf("ruleset does not keep DNS reachable (%q missing)", want)
		}
	}
}

// Replies to an allowed outbound connection must not be mistaken for fresh
// inbound traffic, so established flows are accepted before anything else.
func TestAllowEgressRules_EstablishedAcceptedFirst(t *testing.T) {
	rules := allowEgressRules("10.88.0.0/24", "tap0", []string{"1.2.3.4/32"})
	lines := strings.Split(strings.TrimSpace(rules), "\n")
	var ctIdx, dropIdx = -1, -1
	for i, l := range lines {
		if strings.Contains(l, "ct state established,related accept") {
			ctIdx = i
		}
		if strings.Contains(l, "drop") {
			dropIdx = i
		}
	}
	if ctIdx < 0 {
		t.Fatal("no established,related accept rule")
	}
	if dropIdx >= 0 && ctIdx > dropIdx {
		t.Error("established flows are evaluated after the drop, so replies are denied")
	}
}
