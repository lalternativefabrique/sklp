package space

import "testing"

// CPU quota/period math now lives in ociproc (ociproc.ResourcesSpec); see
// ociproc/cgroups_linux_test.go. These tests cover the Config adapter.

func TestResourcesSpec_NoLimits(t *testing.T) {
	if res, ok := resourcesSpec(Config{RepoPath: "/repo"}); ok || res != nil {
		t.Errorf("resourcesSpec with no limits = %v, %v; want nil, false", res, ok)
	}
}

func TestResourcesSpec_MemoryOnly(t *testing.T) {
	res, ok := resourcesSpec(Config{MemoryLimit: 512 << 20})
	if !ok {
		t.Fatal("resourcesSpec ok = false, want true")
	}
	if _, hasCPU := res["cpu"]; hasCPU {
		t.Error("cpu subsection present for memory-only limit")
	}
	mem := res["memory"].(map[string]any)
	if mem["limit"].(int64) != 512<<20 {
		t.Errorf("memory.limit = %v, want %d", mem["limit"], 512<<20)
	}
}

func TestResourcesSpec_CPUOnly(t *testing.T) {
	res, ok := resourcesSpec(Config{CPULimit: 2.0})
	if !ok {
		t.Fatal("resourcesSpec ok = false, want true")
	}
	if _, hasMem := res["memory"]; hasMem {
		t.Error("memory subsection present for cpu-only limit")
	}
	cpu := res["cpu"].(map[string]any)
	if cpu["quota"].(int64) != 200000 || cpu["period"].(int64) != 100000 {
		t.Errorf("cpu quota/period = %v/%v, want 200000/100000", cpu["quota"], cpu["period"])
	}
}

func TestResourcesSpec_PidsOnly(t *testing.T) {
	res, ok := resourcesSpec(Config{PidsLimit: 256})
	if !ok {
		t.Fatal("resourcesSpec ok = false, want true")
	}
	if _, hasCPU := res["cpu"]; hasCPU {
		t.Error("cpu subsection present for pids-only limit")
	}
	if _, hasMem := res["memory"]; hasMem {
		t.Error("memory subsection present for pids-only limit")
	}
	pids := res["pids"].(map[string]any)
	if pids["limit"].(int64) != 256 {
		t.Errorf("pids.limit = %v, want 256", pids["limit"])
	}
}

func TestResourcesSpec_AllLimits(t *testing.T) {
	res, ok := resourcesSpec(Config{CPULimit: 1, MemoryLimit: 1 << 20, PidsLimit: 128})
	if !ok {
		t.Fatal("resourcesSpec ok = false, want true")
	}
	for _, k := range []string{"cpu", "memory", "pids"} {
		if _, has := res[k]; !has {
			t.Errorf("missing %q subsection when all limits set", k)
		}
	}
}

func TestConfigWantsLimits(t *testing.T) {
	if (Config{}).wantsLimits() {
		t.Error("empty Config should not want limits")
	}
	if !(Config{CPULimit: 1}).wantsLimits() {
		t.Error("CPULimit set should want limits")
	}
	if !(Config{MemoryLimit: 1}).wantsLimits() {
		t.Error("MemoryLimit set should want limits")
	}
	if !(Config{PidsLimit: 1}).wantsLimits() {
		t.Error("PidsLimit set should want limits")
	}
}

func TestSystemdCgroupPath(t *testing.T) {
	got := systemdCgroupPath("sklp-ci")
	if got != "user.slice:sklp:sklp-ci" {
		t.Errorf("systemdCgroupPath = %q, want user.slice:sklp:sklp-ci", got)
	}
}
