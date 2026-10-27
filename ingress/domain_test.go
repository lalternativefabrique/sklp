package ingress

import "testing"

func TestIsPublicDomain(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"example.com", true},
		{"app.example.fr", true},
		{"*.apps.example.com", true},
		{"localhost", false},
		{"web", false},      // bare hostname
		{"app.test", false}, // dev TLD
		{"svc.local", false},
		{"x.internal", false},
		{"host.localhost", false},
		{"n.lan", false},
		{"*.dev.test", false}, // wildcard over a dev TLD
	}
	for _, tc := range cases {
		if got := IsPublicDomain(tc.host); got != tc.want {
			t.Errorf("IsPublicDomain(%q)=%v want %v", tc.host, got, tc.want)
		}
	}
}

// TestQA_IsPublicDomainRejectsIPLiterals proves an IP is never ACME-eligible:
// dns-01 can never validate an address, so managing it would loop on CA/DNS
// failures.
func TestQA_IsPublicDomainRejectsIPLiterals(t *testing.T) {
	cases := []string{"10.88.0.1", "192.168.1.10", "1.1.1.1", "::1", "2001:db8::1", "*.10.0.0.1"}
	for _, h := range cases {
		if IsPublicDomain(h) {
			t.Errorf("IsPublicDomain(%q) must be false (IP literal)", h)
		}
	}
}
