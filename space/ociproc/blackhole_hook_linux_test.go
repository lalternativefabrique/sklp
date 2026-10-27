//go:build linux

package ociproc

import "testing"

// The blackhole set must cover the cloud metadata link-local range (which
// includes 169.254.169.254) and the three RFC1918 host-LAN ranges.
func TestBlackholeMetadataCIDRs_CoversIMDSAndRFC1918(t *testing.T) {
	want := map[string]bool{
		"169.254.0.0/16": false,
		"10.0.0.0/8":     false,
		"172.16.0.0/12":  false,
		"192.168.0.0/16": false,
	}
	for _, c := range BlackholeMetadataCIDRs {
		if _, ok := want[c]; ok {
			want[c] = true
		}
	}
	for cidr, seen := range want {
		if !seen {
			t.Errorf("blackhole set missing required CIDR %s", cidr)
		}
	}
}

// BlackholeHookSpec must produce a createContainer hook that re-execs the given
// binary with the marker env (so the re-exec wakes up as the hook).
func TestBlackholeHookSpec_ReexecsSelfWithMarker(t *testing.T) {
	spec := BlackholeHookSpec("/usr/bin/sklp")
	cc, ok := spec["createContainer"].([]map[string]any)
	if !ok || len(cc) != 1 {
		t.Fatalf("want one createContainer hook, got %v", spec["createContainer"])
	}
	h := cc[0]
	if h["path"] != "/usr/bin/sklp" {
		t.Errorf("hook path = %v, want /usr/bin/sklp", h["path"])
	}
	envs, _ := h["env"].([]string)
	found := false
	for _, e := range envs {
		if e == blackholeHookEnv+"=1" {
			found = true
		}
	}
	if !found {
		t.Errorf("hook env missing %s=1 marker (got %v)", blackholeHookEnv, envs)
	}
}
