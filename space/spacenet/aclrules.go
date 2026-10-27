//go:build linux

package spacenet

import (
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strings"
)

// SetACLEdges pushes the declared depends_on graph (as caller/target service
// name pairs) to the worker, which reconciles the bridge's east-west forward
// chain toward it — default-deny with only the declared edges open. Called
// once after config resolution and again on every register/unregister (a
// service's Ready replica set changing) so the ruleset never targets a stale
// IP. Empty edges reopens east-west (see programACL) — a Space that never
// declares depends_on, or that removes it, keeps/regains the flat bridge it
// always had; the filter is opt-in, never a silent new default.
func (c *SpaceNet) SetACLEdges(edges []ACLEdge) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("spacenet: %q is closed", c.name)
	}
	if c.worker == nil || c.worker.Process == nil {
		return fmt.Errorf("spacenet: %q has no worker", c.name)
	}
	payload, err := json.Marshal(edges)
	if err != nil {
		return fmt.Errorf("spacenet: marshal acl edges: %w", err)
	}
	rep, serr := c.send(command{Op: "acl", IF: c.bridge, Edges: string(payload)})
	if serr != nil {
		return fmt.Errorf("spacenet: acl: %w", serr)
	}
	if !rep.OK {
		return fmt.Errorf("spacenet: acl: %s", rep.Error)
	}
	return nil
}

// aclTable is the nft table the worker owns for intra-Space east-west ACL. A
// named, space-private table (the worker netns is destroyed on Close) keeps
// the rule self-describing and trivially torn down with the namespace.
const aclTable = "sklp_spacenet_acl"

// aclLogPrefix tags every audited packet in the kernel log:
//
//	journalctl -k | grep sklp-acl-would-drop
const aclLogPrefix = "sklp-acl-would-drop"

// ACLEdge is one allowed east-west edge: a caller service may reach a target
// service. Both are resolved to their live registered IPs (nameTable) before
// rendering, so the ruleset always reflects the CURRENT replica set — not a
// point-in-time snapshot that goes stale on a scale/restart.
type ACLEdge struct {
	From string // caller service name
	To   string // target service name
}

// aclRules renders the bridge forward-chain ACL: default-deny between
// containers on the Space's own bridge, with only the declared edges allowed
// through. Uses the nft BRIDGE family, not ip: a Linux bridge switches
// intra-bridge traffic in the kernel's L2 forwarding path, which the ip-family
// "forward" hook (netfilter's L3 path) never sees unless br_netfilter is
// loaded — a kernel module that requires real root to load, is host-global
// (not scoped to this Space's netns), and this rootless worker can neither
// load nor assume present. The bridge family's own forward hook fires on
// every frame the bridge itself forwards, needing no such module — see the
// nft(8) BRIDGE section. Consequently there is no iifname/oifname bridge-name
// scope guard here (unlike allowEgressRules' tap-scoped rules): in the bridge
// family, iifname/oifname name the PORT (veth) a frame entered/left on, never
// the bridge device itself, so such a guard would never match and silently
// accept everything before the drop is ever reached — the hook is already
// scoped to this Space's own bridge by construction (one bridge per netns).
//
// resolve maps a service name to its currently registered IPs — an edge whose
// caller or target has no live IP yet is skipped (a rule with an empty
// source/dest wedges the whole chain the way an empty gateway backend does,
// see agent.go's gatewayRouteLocked).
//
// ARP must be explicitly accepted, and the drop scoped to `meta protocol ip`
// rather than left unqualified: address resolution on the bridge (a
// container's ARP request for its peer's MAC, needed before ANY IP packet —
// including one this ruleset means to allow — can even be framed) also
// traverses this same forward hook. An unqualified drop denies ARP too,
// which breaks the declared edges as well as everything else — confirmed
// against a live kernel: an unqualified `drop` blocked a declared caller/
// target pair identically to an undeclared one, because neither side's ARP
// ever resolved. Established/related is still checked before the edge
// allowlist so a reply on an already-allowed connection is never mistaken
// for a fresh, undeclared one.
//
// An empty edges list renders NOTHING (not an empty default-deny table): a
// Space that declares no depends_on graph keeps the open east-west it always
// had — the filter is opt-in, exactly like allowEgressRules, never a silent
// new default that would break every existing single-purpose Space.
func aclRules(edges []ACLEdge, resolve func(name string) []net.IP) string {
	if len(edges) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "add table bridge %[1]s\n", aclTable)
	fmt.Fprintf(&b, "flush table bridge %[1]s\n", aclTable)
	fmt.Fprintf(&b, "add chain bridge %[1]s forward { type filter hook forward priority -10 ; policy accept ; }\n", aclTable)
	fmt.Fprintf(&b, "add rule bridge %[1]s forward meta protocol arp accept\n", aclTable)
	fmt.Fprintf(&b, "add rule bridge %[1]s forward ct state established,related accept\n", aclTable)
	for _, e := range edges {
		froms := resolve(e.From)
		tos := resolve(e.To)
		if len(froms) == 0 || len(tos) == 0 {
			continue
		}
		for _, from := range froms {
			for _, to := range tos {
				fmt.Fprintf(&b, "add rule bridge %[1]s forward ip saddr %[2]s ip daddr %[3]s accept\n",
					aclTable, from.String(), to.String())
			}
		}
	}
	// Audit only: what a default-deny WOULD drop is logged to the kernel ring
	// buffer and then falls through to the chain's accept policy. Nothing is
	// cut. The log line is how an operator learns what actually crosses this
	// bridge — the gateway's own path in — before any rule is allowed to deny
	// it. Flipping this to a drop is the enforce step, taken only once a real
	// space has run clean under this audit.
	fmt.Fprintf(&b, "add rule bridge %[1]s forward meta protocol ip log prefix \"%[2]s \" level info\n", aclTable, aclLogPrefix)
	return b.String()
}

// programACL runs INSIDE the worker (space netns). It decodes edgesJSON (the
// depends_on graph resolved to []ACLEdge by the host side) and reconciles the
// bridge's forward-chain ACL toward it. Called on "acl" (an explicit push
// from the host after config resolution) and re-applied on every register /
// unregister (a scale event or restart changes which IPs the edges resolve
// to) so the ruleset never lags behind the live replica set. An error is
// returned so the caller can log it.
func programACL(bridge, edgesJSON string, names *nameTable) error {
	var edges []ACLEdge
	if edgesJSON != "" {
		if err := json.Unmarshal([]byte(edgesJSON), &edges); err != nil {
			return fmt.Errorf("decode acl edges: %w", err)
		}
	}
	script := aclRules(edges, func(name string) []net.IP {
		ips, _ := names.lookup(name)
		return ips
	})
	if script == "" {
		// No edges declared: flush any prior ACL table so a config that DROPS
		// its depends_on graph reopens east-west rather than leaving a stale
		// default-deny table behind — the ACL is opt-in on the way in, and
		// must be opt-out symmetrically on the way out.
		script = fmt.Sprintf("add table bridge %[1]s\nflush table bridge %[1]s\ndelete table bridge %[1]s\n", aclTable)
	}
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nft acl %s: %w (%s)", bridge, err, strings.TrimSpace(string(out)))
	}
	return nil
}
