package spacemanager

import (
	"fmt"
	"net"
	"strings"
)

// normalizeAllowEgress validates a space's Internet allowlist and canonicalises
// each entry to CIDR form, so a bare address may be written instead of a /32.
//
// An unparsable entry is an error rather than a skip: silently dropping one
// would leave the space believing it filters a destination it does not, which
// is the failure an allowlist exists to prevent.
func normalizeAllowEgress(entries []string) ([]string, error) {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if _, _, err := net.ParseCIDR(e); err == nil {
			out = append(out, e)
			continue
		}
		if ip := net.ParseIP(e); ip != nil && ip.To4() != nil {
			out = append(out, ip.String()+"/32")
			continue
		}
		return nil, fmt.Errorf("allow_egress %q is not an IPv4 address or CIDR", e)
	}
	return out, nil
}
