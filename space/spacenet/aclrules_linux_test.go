//go:build linux

package spacenet

import (
	"net"
	"strings"
	"testing"
)

func fakeResolve(table map[string][]string) func(string) []net.IP {
	return func(name string) []net.IP {
		var out []net.IP
		for _, s := range table[name] {
			out = append(out, net.ParseIP(s))
		}
		return out
	}
}

// No declared edges must render no rules at all: east-west stays open, so an
// existing Space with no depends_on graph is untouched by the feature's mere
// existence.
func TestACLRules_EmptyRendersNothing(t *testing.T) {
	if got := aclRules(nil, fakeResolve(nil)); got != "" {
		t.Errorf("no edges rendered rules:\n%s", got)
	}
}

// The ruleset must use the nft BRIDGE family, not ip: a Linux bridge switches
// intra-bridge traffic in the kernel's L2 forwarding path, which the ip-family
// forward hook never sees unless br_netfilter is loaded (real root, host-wide,
// unavailable to a rootless worker). Verified end-to-end against a real
// kernel: an ip-family table with this exact ruleset renders successfully but
// silently filters NOTHING (confirmed against a live bridge on a
// br_netfilter-less host) — this assertion is the regression guard for that.
func TestACLRules_UsesBridgeFamily(t *testing.T) {
	resolve := fakeResolve(map[string][]string{"app-x": {"10.88.0.2"}, "nats": {"10.88.0.3"}})
	rules := aclRules([]ACLEdge{{From: "app-x", To: "nats"}}, resolve)
	for _, want := range []string{
		"add table bridge " + aclTable,
		"add chain bridge " + aclTable,
		"add rule bridge " + aclTable,
	} {
		if !strings.Contains(rules, want) {
			t.Errorf("missing bridge-family clause %q\nfull ruleset:\n%s", want, rules)
		}
	}
	if strings.Contains(rules, "table ip "+aclTable) {
		t.Fatalf("rendered an ip-family table, which the bridge forward path never traverses:\n%s", rules)
	}
}

// The audit log must come last, and every declared edge before it: nft
// evaluates in order, so a log placed first would record allowed traffic as
// would-be-dropped. And in audit mode NOTHING may drop, anywhere: the whole
// point is to observe a real space without cutting a single flow.
func TestACLRules_AuditLogIsLastAndNothingDrops(t *testing.T) {
	resolve := fakeResolve(map[string][]string{
		"app-x": {"10.88.0.2"},
		"nats":  {"10.88.0.3"},
	})
	rules := aclRules([]ACLEdge{{From: "app-x", To: "nats"}}, resolve)
	lines := strings.Split(strings.TrimSpace(rules), "\n")

	last := lines[len(lines)-1]
	if !strings.Contains(last, `log prefix "`+aclLogPrefix+` "`) {
		t.Fatalf("last rule is %q, want the audit log", last)
	}
	for i, l := range lines {
		if strings.Contains(l, " drop") {
			t.Errorf("line %d drops traffic in audit mode: %q", i, l)
		}
	}
	if !strings.Contains(rules, "policy accept") {
		t.Errorf("chain policy must stay accept in audit mode:\n%s", rules)
	}
	if !strings.Contains(rules, "10.88.0.2") || !strings.Contains(rules, "10.88.0.3") {
		t.Errorf("declared edge addresses missing from the ruleset:\n%s", rules)
	}
}

// A declared edge whose caller or target has no live registered IP yet must
// be skipped, not rendered with an empty saddr/daddr — an empty match would
// either wedge the whole ruleset or (worse) accept everything.
func TestACLRules_SkipsUnresolvedEdge(t *testing.T) {
	resolve := fakeResolve(map[string][]string{
		"app-x": {"10.88.0.2"},
		// "nats" has no registered IP yet (not Ready).
	})
	rules := aclRules([]ACLEdge{{From: "app-x", To: "nats"}}, resolve)
	if strings.Contains(rules, "ip saddr 10.88.0.2 ip daddr  ") {
		t.Fatalf("rendered an edge with an empty destination:\n%s", rules)
	}
	// Still renders the base chain (established + audit log), just no accept
	// rule for the unresolved edge.
	if !strings.Contains(rules, "log prefix") {
		t.Fatalf("base chain missing even with an unresolved edge:\n%s", rules)
	}
}

// A service with multiple Ready replicas must get an edge per (source IP,
// dest IP) pair — every replica of the caller can reach every replica of the
// target, matching what DNS round-robin already exposes to it.
func TestACLRules_MultiReplicaCrossProduct(t *testing.T) {
	resolve := fakeResolve(map[string][]string{
		"app-x": {"10.88.0.2", "10.88.0.4"},
		"nats":  {"10.88.0.3", "10.88.0.5"},
	})
	rules := aclRules([]ACLEdge{{From: "app-x", To: "nats"}}, resolve)
	for _, pair := range [][2]string{
		{"10.88.0.2", "10.88.0.3"}, {"10.88.0.2", "10.88.0.5"},
		{"10.88.0.4", "10.88.0.3"}, {"10.88.0.4", "10.88.0.5"},
	} {
		want := "ip saddr " + pair[0] + " ip daddr " + pair[1] + " accept"
		if !strings.Contains(rules, want) {
			t.Errorf("missing cross-product rule %q\nfull ruleset:\n%s", want, rules)
		}
	}
}

// A service NOT named as a target of any edge must never appear as a daddr —
// this is the isolation guarantee the whole feature exists for: app-y, never
// declared as a target, must not be reachable even though it shares the
// bridge with app-x and nats.
func TestACLRules_UndeclaredServiceNeverReachable(t *testing.T) {
	resolve := fakeResolve(map[string][]string{
		"app-x": {"10.88.0.2"},
		"app-y": {"10.88.0.6"},
		"nats":  {"10.88.0.3"},
	})
	rules := aclRules([]ACLEdge{{From: "app-x", To: "nats"}}, resolve)
	if strings.Contains(rules, "10.88.0.6") {
		t.Fatalf("app-y's address appears in the ruleset despite no declared edge:\n%s", rules)
	}
}

// ARP must be explicitly accepted before the terminal rule, and that rule
// must be scoped to `meta protocol ip` — not left bare. Confirmed against a
// live kernel when the terminal rule was a drop: unqualified, it blocked
// address resolution on the bridge (a container's ARP request for its peer's
// MAC), which broke EVERY declared edge identically to an undeclared one. The
// same ordering and scope are kept under audit so the enforce flip changes one
// verb, not the ruleset's shape — and so ARP is never logged as a would-drop.
func TestACLRules_ARPAcceptedBeforeIPLog(t *testing.T) {
	resolve := fakeResolve(map[string][]string{"app-x": {"10.88.0.2"}, "nats": {"10.88.0.3"}})
	rules := aclRules([]ACLEdge{{From: "app-x", To: "nats"}}, resolve)
	arpIdx := strings.Index(rules, "meta protocol arp accept")
	logIdx := strings.Index(rules, "meta protocol ip log")
	if arpIdx == -1 {
		t.Fatal("missing ARP accept rule — address resolution would be logged as a would-drop")
	}
	if logIdx == -1 {
		t.Fatal("audit rule is not scoped to `meta protocol ip` — it would also log ARP")
	}
	if arpIdx > logIdx {
		t.Fatalf("ARP accept (%d) comes after the IP audit log (%d)", arpIdx, logIdx)
	}
}

// Established/related flows must be accepted before the edge allowlist is
// evaluated, so a reply on an already-allowed connection is never mistaken
// for a fresh, undeclared one — nor logged as one under audit.
func TestACLRules_EstablishedAcceptedFirst(t *testing.T) {
	resolve := fakeResolve(map[string][]string{"app-x": {"10.88.0.2"}, "nats": {"10.88.0.3"}})
	rules := aclRules([]ACLEdge{{From: "app-x", To: "nats"}}, resolve)
	estIdx := strings.Index(rules, "ct state established,related accept")
	logIdx := strings.Index(rules, " log prefix")
	if estIdx == -1 {
		t.Fatal("missing established,related accept rule")
	}
	if logIdx == -1 {
		t.Fatal("missing audit log rule")
	}
	if estIdx > logIdx {
		t.Fatalf("established rule (%d) comes after the audit log (%d)", estIdx, logIdx)
	}
}
