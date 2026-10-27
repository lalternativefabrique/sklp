package mesh

import (
	"strings"
	"testing"
)

const validFP = "AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89"

func TestCheckAuthority(t *testing.T) {
	if err := CheckAuthority("https://10.73.0.10:4300", validFP); err != nil {
		t.Fatalf("pinned https authority refused: %v", err)
	}
	for name, tc := range map[string]struct{ url, fp, want string }{
		"http with a pin":    {"http://10.73.0.10:4300", validFP, "https"},
		"http without a pin": {"http://10.73.0.10:4300", "", "https"},
		"https without pin":  {"https://10.73.0.10:4300", "", "fingerprint"},
		"truncated pin":      {"https://10.73.0.10:4300", "ab:cd", "SHA-256"},
		"not a url":          {"10.73.0.10:4300", validFP, "valid URL"},
	} {
		err := CheckAuthority(tc.url, tc.fp)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one mentioning %q", name, err, tc.want)
		}
	}
}
