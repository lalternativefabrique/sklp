package mesh

import (
	"encoding/hex"
	"fmt"
	"net/url"
)

// CheckAuthority refuses any meshauthority address the token would reach
// unauthenticated or in clear: the scheme must be https and the certificate
// pinned. An http:// URL skips the pin silently, so a fingerprint next to one
// used to look like protection while the token crossed the network readable.
func CheckAuthority(rawURL, certFingerprint string) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("mesh authority URL %q is not a valid URL", rawURL)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("mesh authority URL %q must use https://: over %s the mesh token crosses the network in clear", rawURL, u.Scheme)
	}
	fp := normaliseFingerprint(certFingerprint)
	if fp == "" {
		return fmt.Errorf("mesh authority %q has no certificate fingerprint: its self-signed certificate cannot be verified without one", rawURL)
	}
	if b, err := hex.DecodeString(fp); err != nil || len(b) != 32 {
		return fmt.Errorf("mesh certificate fingerprint %q is not a SHA-256 in hex", certFingerprint)
	}
	return nil
}
