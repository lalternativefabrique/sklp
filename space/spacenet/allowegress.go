//go:build linux

package spacenet

import (
	"fmt"
	"strings"
)

// allowEgressRules renders the nft forward chain that turns a space's Internet
// egress into an allowlist. An empty list renders nothing at all, so a space
// that declares no allowlist keeps the open egress it had — the filter is
// opt-in, never a silent new default.
//
// The chain only governs traffic LEAVING via the slirp tap. East-west traffic
// on the bridge never matches oifname, so services keep reaching each other by
// name whatever the allowlist says. Established flows are accepted first so a
// reply is not mistaken for a fresh outbound connection.
//
// DNS is exempt: the space resolver runs on the bridge gateway, in this very
// netns, and its upstream queries leave through the tap. Dropping them would
// break name resolution for the allowed destinations themselves, so the
// allowlist would deny what it was told to permit.
func allowEgressRules(subnet, tap string, allow []string) string {
	if len(allow) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "add chain ip %[1]s forward { type filter hook forward priority 0 ; policy accept ; }\n", egressTable)
	fmt.Fprintf(&b, "add rule ip %[1]s forward ct state established,related accept\n", egressTable)
	fmt.Fprintf(&b, "add rule ip %[1]s forward oifname \"%[2]s\" udp dport 53 accept\n", egressTable, tap)
	fmt.Fprintf(&b, "add rule ip %[1]s forward oifname \"%[2]s\" tcp dport 53 accept\n", egressTable, tap)
	for _, cidr := range allow {
		fmt.Fprintf(&b, "add rule ip %[1]s forward ip saddr %[2]s oifname \"%[3]s\" ip daddr %[4]s accept\n",
			egressTable, subnet, tap, cidr)
	}
	// The drop is last and matches only what leaves through the tap, so the
	// policy stays accept for every other hook user while this space's outbound
	// traffic is default-deny.
	fmt.Fprintf(&b, "add rule ip %[1]s forward ip saddr %[2]s oifname \"%[3]s\" drop\n", egressTable, subnet, tap)
	return b.String()
}
