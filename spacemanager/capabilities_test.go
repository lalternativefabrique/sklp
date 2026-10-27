package spacemanager

import (
	"strings"
	"testing"
)

// A typo left to the launcher grants nothing at all: the service starts, then
// fails at the one operation the capability was added for. Catching it at parse
// time points at the manifest that introduced it.
func TestValidateCapabilities_RejectsUnknown(t *testing.T) {
	err := validateCapabilities("service", "core", []string{"NET_BIND_SERVICE", "NET_BIND_SERVIC"})
	if err == nil {
		t.Fatal("a misspelled capability was accepted")
	}
	if !strings.Contains(err.Error(), "NET_BIND_SERVIC") {
		t.Errorf("the error does not name the offending entry: %v", err)
	}
}

// The user-namespace remap is the only thing between a service and the host, so
// no manifest may ask for the capability that undoes it.
func TestValidateCapabilities_RefusesSysAdmin(t *testing.T) {
	if err := validateCapabilities("service", "core", []string{"SYS_ADMIN"}); err == nil {
		t.Error("a manifest was allowed to request CAP_SYS_ADMIN")
	}
}

// Both spellings are accepted, and normalisation settles on the OCI one.
func TestNormalizedCapabilities_AcceptsBothSpellings(t *testing.T) {
	got := normalizedCapabilities([]string{"NET_BIND_SERVICE", "cap_net_admin"})
	if len(got) != 2 || got[0] != "CAP_NET_BIND_SERVICE" || got[1] != "CAP_NET_ADMIN" {
		t.Errorf("normalizedCapabilities = %v, want the CAP_ spelling of both", got)
	}
}

// The common case declares nothing, and must stay nothing.
func TestNormalizedCapabilities_EmptyStaysEmpty(t *testing.T) {
	if got := normalizedCapabilities(nil); got != nil {
		t.Errorf("normalizedCapabilities(nil) = %v, want nil", got)
	}
}

// Spelled right, still refused: these reach the node through the userns.
func TestValidateCapabilities_RefusesHostReachingCaps(t *testing.T) {
	for _, c := range []string{"DAC_READ_SEARCH", "SYS_PTRACE", "NET_ADMIN", "MKNOD", "SYS_CHROOT", "SETPCAP", "SETFCAP"} {
		err := validateCapabilities("service", "core", []string{c})
		if err == nil || !strings.Contains(err.Error(), "cannot be granted by a manifest") {
			t.Errorf("%s: want manifest refusal, got %v", c, err)
		}
	}
	if err := validateCapabilities("service", "core", []string{"NET_BIND_SERVICE", "NET_RAW"}); err != nil {
		t.Errorf("grantable caps refused: %v", err)
	}
}
