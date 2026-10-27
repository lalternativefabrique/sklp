package publicnic

import (
	"fmt"
	"regexp"
	"strings"
)

const NftTable = "sklp-smtp-egress"

const privateRanges = "{ 10.0.0.0/8, 127.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16 }"

// NftScript is applied with one `nft -f`, a single transaction: the flush and
// the re-add commit together, so no packet sees the table half-built.
func NftScript(d Desired) string {
	return strings.Join([]string{
		fmt.Sprintf("table ip %s {}", NftTable),
		fmt.Sprintf("flush table ip %s", NftTable),
		fmt.Sprintf("table ip %s {", NftTable),
		"  chain output { type route hook output priority mangle; policy accept; }",
		"  chain postrouting { type nat hook postrouting priority srcnat; policy accept; }",
		"}",
		fmt.Sprintf("add rule ip %s output ip daddr != %s tcp dport 25 meta mark set %#x", NftTable, privateRanges, d.Mark),
		fmt.Sprintf("add rule ip %s postrouting oifname %q snat to %s", NftTable, d.Interface, d.Source),
		"",
	}, "\n")
}

// NftMissing compares an `nft list table` listing with the desired rules;
// an empty listing means the table does not exist.
func NftMissing(listing string, d Desired) []string {
	if strings.TrimSpace(listing) == "" {
		return []string{"table"}
	}
	checks := []struct {
		name string
		re   string
	}{
		{"output-chain", `hook output priority mangle`},
		{"smtp-mark", fmt.Sprintf(`ip daddr != \{[^}]*\}.*tcp dport 25 meta mark set 0x0*%x\b`, d.Mark)},
		{"postrouting-chain", `type nat hook postrouting priority srcnat`},
		{"snat", fmt.Sprintf(`oifname "%s" snat (ip )?to %s\b`, regexp.QuoteMeta(d.Interface), regexp.QuoteMeta(d.Source.String()))},
	}
	var missing []string
	for _, c := range checks {
		if !regexp.MustCompile(c.re).MatchString(listing) {
			missing = append(missing, c.name)
		}
	}
	return missing
}
