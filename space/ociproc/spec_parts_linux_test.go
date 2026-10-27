//go:build linux

package ociproc

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// IDMappings must be the canonical rootless mapping: container root → caller's
// host uid/gid, then container ids 1..65535 → the ambient 100000..165535 subid
// range. A drift here silently weakens host containment, so it is pinned.
func TestIDMappings(t *testing.T) {
	uid, gid := IDMappings(1000, 1000)

	wantUID := []map[string]any{
		{"containerID": 0, "hostID": 1000, "size": 1},
		{"containerID": 1, "hostID": 100000, "size": 65536},
	}
	if !mappingsEqual(uid, wantUID) {
		t.Errorf("uid mappings = %v, want %v", uid, wantUID)
	}
	// gid mirrors uid with the gid host id at root.
	if gid[0]["hostID"] != 1000 || gid[1]["hostID"] != 100000 || gid[1]["size"] != 65536 {
		t.Errorf("gid mappings wrong: %v", gid)
	}
	// Host root id 0 maps a SINGLE id (size 1) — never a range, or the host
	// root would be reachable from inside.
	if uid[0]["size"] != 1 {
		t.Errorf("root mapping size = %v, want 1", uid[0]["size"])
	}
}

func TestResolveIDMappingsReadsSubIDFiles(t *testing.T) {
	dir := t.TempDir()
	oldUIDPath, oldGIDPath := subuidPath, subgidPath
	subuidPath = filepath.Join(dir, "subuid")
	subgidPath = filepath.Join(dir, "subgid")
	t.Cleanup(func() {
		subuidPath = oldUIDPath
		subgidPath = oldGIDPath
	})

	name, err := currentSubIDName(1000)
	if err != nil {
		t.Fatalf("currentSubIDName: %v", err)
	}
	if err := os.WriteFile(subuidPath, []byte(name+":200000:70000\n"), 0o644); err != nil {
		t.Fatalf("write subuid: %v", err)
	}
	if err := os.WriteFile(subgidPath, []byte(name+":300000:80000\n"), 0o644); err != nil {
		t.Fatalf("write subgid: %v", err)
	}

	uid, gid, err := ResolveIDMappings(1000, 1000)
	if err != nil {
		t.Fatalf("ResolveIDMappings: %v", err)
	}
	if uid[1]["hostID"] != 200000 || uid[1]["size"] != 70000 {
		t.Fatalf("uid subid mapping = %v", uid)
	}
	if gid[1]["hostID"] != 300000 || gid[1]["size"] != 80000 {
		t.Fatalf("gid subid mapping = %v", gid)
	}
}

func TestResolveIDMappingsFailsWhenSubIDMissing(t *testing.T) {
	dir := t.TempDir()
	oldUIDPath, oldGIDPath := subuidPath, subgidPath
	subuidPath = filepath.Join(dir, "subuid")
	subgidPath = filepath.Join(dir, "subgid")
	t.Cleanup(func() {
		subuidPath = oldUIDPath
		subgidPath = oldGIDPath
	})
	if err := os.WriteFile(subuidPath, []byte("other:200000:70000\n"), 0o644); err != nil {
		t.Fatalf("write subuid: %v", err)
	}
	if err := os.WriteFile(subgidPath, []byte("other:300000:80000\n"), 0o644); err != nil {
		t.Fatalf("write subgid: %v", err)
	}

	if _, _, err := ResolveIDMappings(1000, 1000); err == nil {
		t.Fatal("expected missing subid range to fail")
	}
}

// DefaultCapabilities must NEVER include CAP_SYS_ADMIN — the userns remap is the
// only host containment, and SYS_ADMIN would defeat it. This is the load-bearing
// security assertion of the shared baseline.
func TestDefaultCapabilities_NoSysAdmin(t *testing.T) {
	caps := DefaultCapabilities()
	if slices.Contains(caps, "CAP_SYS_ADMIN") {
		t.Fatal("DefaultCapabilities must NOT grant CAP_SYS_ADMIN")
	}
	// A couple of caps the toolchain genuinely needs, as a regression guard.
	for _, need := range []string{"CAP_CHOWN", "CAP_SETUID", "CAP_DAC_OVERRIDE"} {
		if !slices.Contains(caps, need) {
			t.Errorf("DefaultCapabilities missing %s", need)
		}
	}
}

// DefaultCapabilities returns a FRESH slice each call, so a caller appending an
// extra cap (the build path's CAP_NET_ADMIN) cannot mutate the shared baseline.
func TestDefaultCapabilities_FreshSlice(t *testing.T) {
	a := DefaultCapabilities()
	_ = append(a, "CAP_NET_ADMIN") //nolint:gocritic // intentional aliasing probe
	b := DefaultCapabilities()
	if slices.Contains(b, "CAP_NET_ADMIN") {
		t.Fatal("appending to one DefaultCapabilities() result leaked into the next — not a fresh slice")
	}
}

func mappingsEqual(a, b []map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i]["containerID"] != b[i]["containerID"] || a[i]["hostID"] != b[i]["hostID"] || a[i]["size"] != b[i]["size"] {
			return false
		}
	}
	return true
}

// TestDefaultSeccomp_ShapeAndPolicy pins the seccomp profile contract: an
// allowlist (defaultAction = ERRNO, not KILL) with multi-arch coverage and a
// single ALLOW block. ERRNO over KILL keeps a step that probes a denied syscall
// limping instead of crashing.
func TestDefaultSeccomp_ShapeAndPolicy(t *testing.T) {
	sc := DefaultSeccomp()
	if got := sc["defaultAction"]; got != "SCMP_ACT_ERRNO" {
		t.Errorf("defaultAction = %v, want SCMP_ACT_ERRNO (errno, not kill)", got)
	}
	arches, _ := sc["architectures"].([]string)
	if !slices.Contains(arches, "SCMP_ARCH_X86_64") || !slices.Contains(arches, "SCMP_ARCH_AARCH64") {
		t.Errorf("architectures must cover x86_64 + aarch64, got %v", arches)
	}
	rules, _ := sc["syscalls"].([]map[string]any)
	if len(rules) != 1 || rules[0]["action"] != "SCMP_ACT_ALLOW" {
		t.Fatalf("expected one ALLOW rule block, got %v", rules)
	}
}

// TestDefaultSeccomp_UsernsFriendly is the load-bearing assertion: the profile
// must NOT deny the syscalls sklp containers genuinely need inside their user
// namespace (clone/unshare/mount/keyctl for dpkg/apt/slirp), and MUST omit the
// host-escape syscalls that have no business in a shared-kernel step.
func TestDefaultSeccomp_UsernsFriendly(t *testing.T) {
	allowed := allowSet(t)
	for _, need := range []string{"clone", "clone3", "unshare", "mount", "umount2", "keyctl", "setns", "pivot_root", "chroot"} {
		if need == "pivot_root" {
			// runc uses pivot_root itself before seccomp loads; not required in-list.
			continue
		}
		if !allowed[need] {
			t.Errorf("allowlist is missing %q — build/userns path will break", need)
		}
	}
	for _, denied := range []string{"kexec_load", "init_module", "finit_module", "delete_module", "bpf", "ptrace", "reboot", "swapon", "swapoff", "mount_setattr", "open_tree", "move_mount"} {
		if allowed[denied] {
			t.Errorf("allowlist must NOT contain host-escape syscall %q", denied)
		}
	}
}

func allowSet(t *testing.T) map[string]bool {
	t.Helper()
	sc := DefaultSeccomp()
	rules := sc["syscalls"].([]map[string]any)
	names := rules[0]["names"].([]string)
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// TestDefaultMaskedPaths_CoversDangerousProcSys pins the masked + readonly path
// sets: kcore/sysrq-trigger/firmware masked, /proc/sys read-only.
func TestDefaultMaskedPaths_CoversDangerousProcSys(t *testing.T) {
	masked := DefaultMaskedPaths()
	for _, p := range []string{"/proc/kcore", "/proc/sysrq-trigger", "/sys/firmware"} {
		if !slices.Contains(masked, p) {
			t.Errorf("masked paths missing %q", p)
		}
	}
	if ro := DefaultReadonlyPaths(); !slices.Contains(ro, "/proc/sys") {
		t.Errorf("readonly paths missing /proc/sys, got %v", ro)
	}
}
