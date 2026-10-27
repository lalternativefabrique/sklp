//go:build linux

package ociproc

import (
	"strings"
	"testing"
)

// The point of the service baseline: what a shared node must not hand to every
// service. NET_RAW is the one that matters — it forges packets and ARP-spoofs
// inside the space bridge, so one service can impersonate another of the same
// space, on the traffic carrying the database password.
func TestServiceCapabilities_DropsWhatAServiceMustNotHave(t *testing.T) {
	got := strings.Join(ServiceCapabilities(), " ")
	for _, unwanted := range []string{
		"CAP_NET_RAW", "CAP_MKNOD", "CAP_SYS_CHROOT",
		"CAP_SETPCAP", "CAP_SETFCAP", "CAP_AUDIT_WRITE", "CAP_SYS_ADMIN",
	} {
		if strings.Contains(got, unwanted) {
			t.Errorf("the service baseline still grants %s", unwanted)
		}
	}
}

// It must stay wide enough for a normal image's entrypoint, which chowns the
// data dir it was handed and drops to its own user before serving.
func TestServiceCapabilities_KeepsWhatAnEntrypointNeeds(t *testing.T) {
	got := strings.Join(ServiceCapabilities(), " ")
	for _, needed := range []string{
		"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FOWNER", "CAP_FSETID",
		"CAP_SETGID", "CAP_SETUID", "CAP_KILL",
	} {
		if !strings.Contains(got, needed) {
			t.Errorf("the service baseline lost %s, which a normal entrypoint needs", needed)
		}
	}
}

// The CI runner keeps its own, wider set: it builds, installs and chowns tooling
// caches. Narrowing a service must not narrow a build.
func TestDefaultCapabilities_UnchangedForTheRunner(t *testing.T) {
	if !strings.Contains(strings.Join(DefaultCapabilities(), " "), "CAP_NET_RAW") {
		t.Error("the runner baseline changed; only the service one was meant to narrow")
	}
}

func TestNormalizeCapability(t *testing.T) {
	for in, want := range map[string]string{
		"NET_RAW":            "CAP_NET_RAW",
		"cap_net_raw":        "CAP_NET_RAW",
		" NET_BIND_SERVICE ": "CAP_NET_BIND_SERVICE",
		"SYS_ADMIN":          "",
		"nonsense":           "",
		"":                   "",
	} {
		if got := NormalizeCapability(in); got != want {
			t.Errorf("NormalizeCapability(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestManifestGrantableCapability_RefusesHostReachingCaps(t *testing.T) {
	for _, c := range []string{"DAC_READ_SEARCH", "cap_sys_ptrace", "NET_ADMIN", "MKNOD", "SYS_CHROOT", "SETPCAP", "SETFCAP", "SYS_ADMIN"} {
		if got := ManifestGrantableCapability(c); got != "" {
			t.Errorf("ManifestGrantableCapability(%q) = %q, want refused", c, got)
		}
	}
	for _, c := range []string{"NET_BIND_SERVICE", "NET_RAW", "CHOWN", "SYS_NICE", "IPC_LOCK"} {
		if got := ManifestGrantableCapability(c); got == "" {
			t.Errorf("ManifestGrantableCapability(%q) refused, want granted", c)
		}
	}
}
