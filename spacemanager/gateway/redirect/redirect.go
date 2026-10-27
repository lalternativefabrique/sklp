// Package redirect installs the host nftables NAT that fronts the rootless
// gateway: external :80/:443 redirected to the unprivileged :9080/:9443 the
// gateway singleton (or the mux) actually binds.
//
// WHY THIS IS NEEDED: sklp runs the gateway rootless, so it binds the
// unprivileged 9080/9443 (no CAP_NET_BIND_SERVICE). The public internet speaks
// to 80/443, so a NAT redirect bridges the two. Port redirection is a NAT
// operation — only nftables/iptables can do it (a firewall like ufw only
// opens/closes a port, it cannot remap one). This wiring was previously a manual
// step documented as "outside the runtime"; this package makes it a first-class
// `sklp gateway redirect` verb so the operator never hand-writes nft rules.
//
// The whole ruleset is one named table (redirectTable) so install is idempotent
// (flush+re-add) and remove is a single table delete — it never touches the
// operator's other nftables state.
package redirect

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// redirectTable is the dedicated nft table holding the front-door redirects.
// Named (not shared) so remove deletes exactly this and nothing else.
const redirectTable = "sklp_gateway_redirect"

// persistPath is where the ruleset is written so it survives a reboot. The
// standard nftables service sources /etc/nftables.conf, which conventionally
// includes /etc/nftables.d/*.conf — dropping our file there means the table is
// reinstated on boot without editing the operator's main config.
const persistPath = "/etc/nftables.d/sklp-gateway-redirect.conf"

// Front-door port mapping: the public port the world hits -> the unprivileged
// port the rootless gateway binds. Kept here (not imported from the traefik
// package) so the redirect is driver-agnostic — the mux uses the same ports.
const (
	publicHTTP   = 80
	publicHTTPS  = 443
	gatewayHTTP  = 9080
	gatewayHTTPS = 9443
)

// Mapping is one public->gateway port redirect. Exposed so the renderer and the
// status check share one source of truth and a test can assert the set.
type Mapping struct {
	Public  int
	Gateway int
}

// DefaultMappings is the standard front-door set (:80->:9080, :443->:9443).
func DefaultMappings() []Mapping {
	return []Mapping{
		{Public: publicHTTP, Gateway: gatewayHTTP},
		{Public: publicHTTPS, Gateway: gatewayHTTPS},
	}
}

// renderInstall builds the atomic nft script that (re)installs the redirect
// table: drop any prior copy (idempotent), recreate the nat prerouting +
// output chains, and add a redirect rule per mapping. The output chain mirrors
// prerouting for traffic the host originates to itself (a local curl to the
// public IP), so a same-host health check follows the same path as the world.
// Traffic arriving over wg0 is exempted first: mesh traffic is east-west,
// addressed to the Network Controller's own mesh-bound listeners (netctrl),
// and redirecting it into the north-south gateway mux would answer every
// cross-node call with the mux's 404 instead.
func renderInstall(maps []Mapping) string {
	var b strings.Builder
	fmt.Fprintf(&b, "add table ip %[1]s\n", redirectTable)
	fmt.Fprintf(&b, "flush table ip %[1]s\n", redirectTable)
	fmt.Fprintf(&b, "add chain ip %[1]s prerouting { type nat hook prerouting priority -100 ; }\n", redirectTable)
	fmt.Fprintf(&b, "add chain ip %[1]s output { type nat hook output priority -100 ; }\n", redirectTable)
	fmt.Fprintf(&b, "add rule ip %[1]s prerouting iifname \"wg0\" accept\n", redirectTable)
	for _, m := range maps {
		fmt.Fprintf(&b, "add rule ip %[1]s prerouting tcp dport %[2]d redirect to :%[3]d\n", redirectTable, m.Public, m.Gateway)
		fmt.Fprintf(&b, "add rule ip %[1]s output ip daddr 127.0.0.1 tcp dport %[2]d redirect to :%[3]d\n", redirectTable, m.Public, m.Gateway)
	}
	return b.String()
}

// Install programs the front-door redirect for the default mappings. It is
// idempotent (the script flushes the table first) so a second call is a no-op.
// nft needs CAP_NET_ADMIN: run the verb as root (or via sudo). A permission
// error is surfaced verbatim so the operator knows to elevate.
func Install() error {
	return applyScript(renderInstall(DefaultMappings()))
}

// Persist writes the redirect ruleset to persistPath so the standard nftables
// systemd service reinstates it on boot (the in-RAM table from Install is lost
// on reboot otherwise). It does NOT enable the nftables service or edit
// /etc/nftables.conf — it only drops the include file; PersistHint reports what,
// if anything, the operator still needs to wire up. Needs root (writes /etc).
func Persist() error {
	if err := os.MkdirAll(filepath.Dir(persistPath), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(persistPath), err)
	}
	if err := os.WriteFile(persistPath, []byte(renderInstall(DefaultMappings())), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", persistPath, err)
	}
	return nil
}

// PersistPath is where Persist drops the boot-time ruleset (exposed so the CLI
// can tell the operator the exact file and whether the nftables service sources
// it).
func PersistPath() string { return persistPath }

// Unpersist removes the boot-time ruleset file. A missing file is not an error.
func Unpersist() error {
	if err := os.Remove(persistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", persistPath, err)
	}
	return nil
}

// MainConfigSourcesDropIn reports whether /etc/nftables.conf already includes the
// /etc/nftables.d drop-in dir, so the CLI can warn the operator when Persist's
// file would NOT be picked up on boot. Best-effort: an unreadable main config
// reads as "not sourced" (the safe, warn-the-operator default).
func MainConfigSourcesDropIn() bool {
	b, err := os.ReadFile("/etc/nftables.conf")
	if err != nil {
		return false
	}
	return strings.Contains(string(b), "/etc/nftables.d")
}

// Remove deletes the redirect table, undoing every front-door rule in one shot.
// A missing table is not an error (idempotent teardown).
func Remove() error {
	out, err := exec.Command("nft", "delete", "table", "ip", redirectTable).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		// `No such file or directory` == table already gone: treat as success.
		if strings.Contains(msg, "No such file") || strings.Contains(msg, "does not exist") {
			return nil
		}
		return fmt.Errorf("nft delete table %s: %w (%s)", redirectTable, err, msg)
	}
	return nil
}

// State is the tri-state result of querying the redirect: installed, absent, or
// indeterminate because nft could not be queried (typically not root).
type State int

const (
	StateUnknown   State = iota // nft query failed (e.g. needs root) — can't tell
	StateInstalled              // the redirect table is present
	StateAbsent                 // nft ran and the table is not there
)

// Status queries the redirect table, distinguishing a real "absent" (nft ran,
// no such table) from "unknown" (nft itself failed, e.g. listing needs root).
// This keeps `redirect status` honest instead of reporting "not installed" when
// it merely lacked permission to look. The error is the underlying nft failure.
func Status() (State, error) {
	out, err := exec.Command("nft", "list", "table", "ip", redirectTable).CombinedOutput()
	msg := string(out)
	if err == nil {
		if strings.Contains(msg, "redirect to") {
			return StateInstalled, nil
		}
		return StateAbsent, nil
	}
	// nft ran and reported the table simply does not exist: a definite absent.
	if strings.Contains(msg, "No such file") || strings.Contains(msg, "does not exist") {
		return StateAbsent, nil
	}
	// Anything else (permission denied, nft missing): we genuinely can't tell.
	return StateUnknown, fmt.Errorf("nft list table %s: %w (%s)", redirectTable, err, strings.TrimSpace(msg))
}

// applyScript feeds an nft ruleset on stdin (the `nft -f -` form, matching the
// space egress programmer). A non-zero exit returns the nft stderr verbatim.
func applyScript(script string) error {
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nft apply redirect: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}
