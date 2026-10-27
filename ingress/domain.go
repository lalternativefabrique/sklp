package ingress

import (
	"net"
	"strings"
)

// domain.go classifies a route host as public (an ACME-eligible FQDN) or
// private/dev. It is the single source of truth shared by the worker's ACME
// domain reconciliation and the gateway's TLS routing decision.

// privateTLDs are the suffixes that can never pass a public ACME challenge, so
// their hosts stay on self-signed / plain HTTP.
var privateTLDs = []string{".test", ".local", ".internal", ".localhost", ".lan"}

// IsPublicDomain reports whether host is a public FQDN that warrants a real
// (ACME) TLS certificate. Private/dev TLDs (.test, .local, .internal,
// .localhost, .lan) and bare hostnames (no dot) return false — Let's Encrypt can
// never validate them.
func IsPublicDomain(host string) bool {
	if !strings.Contains(host, ".") {
		return false
	}
	// A wildcard managed domain (*.example.com) is public if its base is.
	h := strings.TrimPrefix(host, "*.")
	// An IP literal is never ACME-eligible: dns-01 cannot validate an address,
	// so managing it would burn CA/DNS calls in a permanent-failure loop.
	if net.ParseIP(h) != nil {
		return false
	}
	for _, tld := range privateTLDs {
		if strings.HasSuffix(h, tld) {
			return false
		}
	}
	return true
}
