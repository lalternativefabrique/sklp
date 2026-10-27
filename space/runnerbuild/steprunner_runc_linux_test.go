//go:build linux

package runnerbuild

import (
	"os"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/space/ociproc"
)

// helpers to read the loosely-typed spec map.
func mustBuildStepSpec(t *testing.T, r RuncStepRunner, absRoot, script string, isolatedNet bool, exitDir, selfExe, resolvConf string, resources map[string]any, cgroupsPath string) map[string]any {
	t.Helper()
	uidMappings, gidMappings := ociproc.IDMappingsWithSubIDs(os.Getuid(), os.Getgid(), 200000, 70000, 300000, 80000)
	return r.buildStepSpecWithMappings(absRoot, script, isolatedNet, exitDir, selfExe, resolvConf, resources, cgroupsPath, uidMappings, gidMappings)
}

func specLinux(t *testing.T, spec map[string]any) map[string]any {
	t.Helper()
	l, ok := spec["linux"].(map[string]any)
	if !ok {
		t.Fatal("spec has no linux object")
	}
	return l
}

func nsTypes(t *testing.T, spec map[string]any) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, n := range specLinux(t, spec)["namespaces"].([]map[string]any) {
		out[n["type"].(string)] = true
	}
	return out
}

// Step 2: with isolatedNet the spec MUST declare a private network namespace
// (slirp attaches to it). The user/mount/pid/ipc/uts namespaces are always
// present.
func TestRuncStepSpec_IsolatedNetHasNetworkNamespace(t *testing.T) {
	r := RuncStepRunner{CacheRoot: "/cache"}
	spec := mustBuildStepSpec(t, r, "/abs/rootfs", "true", true, "/tmp/ctl", "/usr/bin/sklp", "/bundle/resolv.conf", nil, "")
	ns := nsTypes(t, spec)
	if !ns["network"] {
		t.Error("isolatedNet=true must declare a private network namespace")
	}
	for _, want := range []string{"user", "mount", "pid", "ipc", "uts"} {
		if !ns[want] {
			t.Errorf("missing %q namespace", want)
		}
	}
}

// Fallback: with isolatedNet=false (no slirp) there must be NO network
// namespace — its absence is what shares the host netns, and the host
// resolv.conf is bound in for DNS.
func TestRuncStepSpec_HostNetFallback(t *testing.T) {
	r := RuncStepRunner{CacheRoot: "/cache"}
	spec := mustBuildStepSpec(t, r, "/abs/rootfs", "true", false, "/tmp/ctl", "/usr/bin/sklp", "/bundle/resolv.conf", nil, "")
	if nsTypes(t, spec)["network"] {
		t.Error("isolatedNet=false must omit the network namespace (host netns)")
	}
	hasResolv := false
	for _, m := range spec["mounts"].([]map[string]any) {
		if m["destination"] == "/etc/resolv.conf" {
			hasResolv = true
		}
	}
	if !hasResolv {
		t.Error("host-net fallback must bind /etc/resolv.conf for DNS")
	}
}

// The isolated-net spec must bind the BUNDLE resolv.conf (pointing at slirp's
// DNS), NOT the host /etc/resolv.conf — the isolated netns can't reach the
// host's resolver.
func TestRuncStepSpec_IsolatedNetUsesSlirpResolv(t *testing.T) {
	r := RuncStepRunner{CacheRoot: "/cache"}
	spec := mustBuildStepSpec(t, r, "/abs/rootfs", "true", true, "/tmp/ctl", "/usr/bin/sklp", "/bundle/resolv.conf", nil, "")
	var src string
	for _, m := range spec["mounts"].([]map[string]any) {
		if m["destination"] == "/etc/resolv.conf" {
			src = m["source"].(string)
		}
	}
	if src == "" {
		t.Fatal("isolated netns must bind a resolv.conf")
	}
	if src == "/etc/resolv.conf" {
		t.Error("isolated netns must NOT bind the host resolv.conf")
	}
	if src != "/bundle/resolv.conf" {
		t.Errorf("resolv.conf source = %q, want the bundle resolv.conf", src)
	}
}

// The exit-status channel: a writable bind from the bundle ctl dir at /run/sklp,
// so the step reports $? without touching the rootfs/layer.
func TestRuncStepSpec_ExitChannelBind(t *testing.T) {
	r := RuncStepRunner{CacheRoot: "/cache"}
	spec := mustBuildStepSpec(t, r, "/abs/rootfs", "true", true, "/bundle/ctl", "/usr/bin/sklp", "/bundle/resolv.conf", nil, "")
	found := false
	for _, m := range spec["mounts"].([]map[string]any) {
		if m["destination"] == "/run/sklp" {
			found = true
			if m["type"] != "bind" || m["source"] != "/bundle/ctl" {
				t.Errorf("/run/sklp mount = %v, want rw bind from /bundle/ctl", m)
			}
			rw := false
			for _, o := range m["options"].([]string) {
				if o == "rw" {
					rw = true
				}
			}
			if !rw {
				t.Errorf("/run/sklp bind must be rw, got %v", m["options"])
			}
		}
	}
	if !found {
		t.Error("/run/sklp exit-channel bind missing")
	}
}

// The isolated-net spec must wire the createContainer blackhole hook (the only
// context that can install the IMDS filter on a rootless netns). The host-net
// fallback must NOT (no netns to filter).
func TestRuncStepSpec_BlackholeHookOnIsolatedOnly(t *testing.T) {
	r := RuncStepRunner{CacheRoot: "/cache"}

	iso := mustBuildStepSpec(t, r, "/abs/rootfs", "true", true, "/tmp/ctl", "/usr/bin/sklp", "/bundle/resolv.conf", nil, "")
	hooks, ok := iso["hooks"].(map[string]any)
	if !ok {
		t.Fatal("isolated-net spec must declare hooks")
	}
	cc, ok := hooks["createContainer"].([]map[string]any)
	if !ok || len(cc) == 0 {
		t.Fatal("isolated-net spec must have a createContainer hook")
	}
	if cc[0]["path"] != "/usr/bin/sklp" {
		t.Errorf("hook path = %v, want the sklp self-exe", cc[0]["path"])
	}

	host := mustBuildStepSpec(t, r, "/abs/rootfs", "true", false, "/tmp/ctl", "/usr/bin/sklp", "/bundle/resolv.conf", nil, "")
	if _, has := host["hooks"]; has {
		t.Error("host-net fallback must not declare a blackhole hook")
	}
}

// cgroup limits (step 3): resources + cgroupsPath land in linux when supplied,
// and are absent (byte-identical to unbounded) when nil/"".
func TestRuncStepSpec_CgroupLimits(t *testing.T) {
	r := RuncStepRunner{CacheRoot: "/cache"}

	bare := specLinux(t, mustBuildStepSpec(t, r, "/abs/rootfs", "true", true, "/c", "/sklp", "/r", nil, ""))
	if _, has := bare["resources"]; has {
		t.Error("linux.resources present with no limits")
	}
	if _, has := bare["cgroupsPath"]; has {
		t.Error("linux.cgroupsPath present with no limit")
	}

	res := map[string]any{"pids": map[string]any{"limit": int64(4096)}}
	lx := specLinux(t, mustBuildStepSpec(t, r, "/abs/rootfs", "true", true, "/c", "/sklp", "/r", res, "user.slice:sklp:x"))
	got, ok := lx["resources"].(map[string]any)
	if !ok || got["pids"] == nil {
		t.Errorf("linux.resources missing pids: %v", lx["resources"])
	}
	if lx["cgroupsPath"] != "user.slice:sklp:x" {
		t.Errorf("cgroupsPath = %v, want user.slice:sklp:x", lx["cgroupsPath"])
	}
}

// A build writes its rootfs (apt installs), so root.readonly must be false —
// the opposite of a service spec.
func TestRuncStepSpec_RootfsWritable(t *testing.T) {
	r := RuncStepRunner{CacheRoot: "/cache"}
	spec := mustBuildStepSpec(t, r, "/abs/rootfs", "true", true, "/tmp/ctl", "/usr/bin/sklp", "/bundle/resolv.conf", nil, "")
	root := spec["root"].(map[string]any)
	if root["readonly"].(bool) {
		t.Error("root.readonly = true; a build step must be able to write its rootfs")
	}
	if root["path"].(string) != "/abs/rootfs" {
		t.Errorf("root.path = %q, want /abs/rootfs", root["path"])
	}
}

// The uid-0 + subuid-range mapping is what lets apt/dpkg chown to service uids;
// it must match the unshare runner's --map-users=1:100000:65535 shape.
func TestRuncStepSpec_SubuidMapping(t *testing.T) {
	r := RuncStepRunner{CacheRoot: "/cache"}
	spec := mustBuildStepSpec(t, r, "/abs/rootfs", "true", true, "/tmp/ctl", "/usr/bin/sklp", "/bundle/resolv.conf", nil, "")
	uidMaps := specLinux(t, spec)["uidMappings"].([]map[string]any)
	if len(uidMaps) != 2 {
		t.Fatalf("want 2 uid mappings (root + subuid range), got %d", len(uidMaps))
	}
	// Second entry: container 1 -> host subuid range read from /etc/subuid.
	sub := uidMaps[1]
	if sub["containerID"] != 1 || sub["hostID"] != 200000 || sub["size"] != 70000 {
		t.Errorf("subuid mapping = %v, want {containerID:1 hostID:200000 size:70000}", sub)
	}
}

// CAP_SYS_ADMIN must never be granted: the userns remap is the only host
// protection and a build step must not escalate past it.
func TestRuncStepSpec_NoSysAdmin(t *testing.T) {
	r := RuncStepRunner{CacheRoot: "/cache"}
	spec := mustBuildStepSpec(t, r, "/abs/rootfs", "true", true, "/tmp/ctl", "/usr/bin/sklp", "/bundle/resolv.conf", nil, "")
	caps := spec["process"].(map[string]any)["capabilities"].(map[string]any)
	for _, set := range []string{"bounding", "effective", "permitted"} {
		for _, c := range caps[set].([]string) {
			if c == "CAP_SYS_ADMIN" {
				t.Errorf("CAP_SYS_ADMIN granted in %s set", set)
			}
		}
	}
}

// Cache mounts must appear as private bind mounts (so go/pnpm caches are warm)
// without their content ever landing in the rootfs/layer.
func TestRuncStepSpec_CacheMountsAsBinds(t *testing.T) {
	r := RuncStepRunner{
		CacheRoot:   "/cache",
		CacheMounts: []CacheMount{{Source: "/host/go-mod", Target: "/root/go/pkg/mod"}},
	}
	spec := mustBuildStepSpec(t, r, "/abs/rootfs", "true", true, "/tmp/ctl", "/usr/bin/sklp", "/bundle/resolv.conf", nil, "")
	found := false
	for _, m := range spec["mounts"].([]map[string]any) {
		if m["destination"] == "/root/go/pkg/mod" {
			found = true
			if m["type"] != "bind" || m["source"] != "/host/go-mod" {
				t.Errorf("cache mount = %v, want bind from /host/go-mod", m)
			}
		}
	}
	if !found {
		t.Error("cache mount /root/go/pkg/mod not present in spec mounts")
	}
}

// The build script must batch all runs under `set -e` (in a sub-shell), export
// the image env after the baked PATH (so the image PATH wins), capture the
// status to the exit file, and run each command.
func TestRuncBuildScript_BatchesUnderSetE(t *testing.T) {
	r := RuncStepRunner{
		ImageEnv: []string{"PATH=/usr/local/go/bin:/usr/bin"},
		ExtraEnv: []string{"GITHUB_TOKEN=secret"},
	}
	script := r.buildScript([]string{"apt-get update", "go build ./..."})
	// The RUNs run in a sub-shell with `set -e`; the env exports precede it.
	if !strings.Contains(script, "(\n  set -e\n") {
		t.Error("RUN batch must run in a `set -e` sub-shell")
	}
	if !strings.Contains(script, "GITHUB_TOKEN") {
		t.Error("ExtraEnv not exported")
	}
	// The exit status must be captured to the exit file so the build can fail.
	if !strings.Contains(script, exitFilePath) {
		t.Errorf("script must write the exit code to %s", exitFilePath)
	}
	// Image PATH export must come AFTER the baked PATH export so it wins.
	bakedIdx := strings.Index(script, "/opt/sklp/tools/bin")
	imageIdx := strings.Index(script, "/usr/local/go/bin")
	if bakedIdx < 0 || imageIdx < 0 || imageIdx < bakedIdx {
		t.Errorf("image PATH must be exported after baked PATH (baked@%d image@%d)", bakedIdx, imageIdx)
	}
	if !strings.Contains(script, "apt-get update") || !strings.Contains(script, "go build ./...") {
		t.Error("not all RUN commands present in script")
	}
}

// WithStage must layer the stage image env + cache mounts onto a copy without
// mutating the receiver (each stage gets its own config).
func TestRuncWithStage_CopiesConfig(t *testing.T) {
	base := RuncStepRunner{CacheRoot: "/cache", CacheMounts: []CacheMount{{Source: "/a", Target: "/a"}}}
	staged := base.WithStage([]string{"FOO=1"}, []CacheMount{{Source: "/b", Target: "/b"}}).(RuncStepRunner)
	if len(base.CacheMounts) != 1 {
		t.Errorf("receiver mutated: base now has %d cache mounts", len(base.CacheMounts))
	}
	if len(staged.CacheMounts) != 2 {
		t.Errorf("staged should have 2 cache mounts, got %d", len(staged.CacheMounts))
	}
	if len(staged.ImageEnv) != 1 || staged.ImageEnv[0] != "FOO=1" {
		t.Errorf("staged ImageEnv = %v, want [FOO=1]", staged.ImageEnv)
	}
}

// TestBuildStepSpec_Hardened gates the build path: every step spec carries the
// shared seccomp allowlist (errno default) + masked/readonly proc paths, on both
// the isolated-net and host-net variants. The build needs mount/chroot/keyctl,
// all already in the shared allowlist, so hardening must not be dropped here.
func TestBuildStepSpec_Hardened(t *testing.T) {
	var r RuncStepRunner
	for _, iso := range []bool{true, false} {
		lx := specLinux(t, mustBuildStepSpec(t, r, "/abs/rootfs", "true", iso, "/c", "/sklp", "/r", nil, ""))
		if _, ok := lx["maskedPaths"]; !ok {
			t.Errorf("build step(iso=%v) missing maskedPaths", iso)
		}
		if _, ok := lx["readonlyPaths"]; !ok {
			t.Errorf("build step(iso=%v) missing readonlyPaths", iso)
		}
		sc, ok := lx["seccomp"].(map[string]any)
		if !ok {
			t.Fatalf("build step(iso=%v) missing seccomp", iso)
		}
		if sc["defaultAction"] != "SCMP_ACT_ERRNO" {
			t.Errorf("build step(iso=%v) seccomp defaultAction = %v, want SCMP_ACT_ERRNO", iso, sc["defaultAction"])
		}
	}
}
