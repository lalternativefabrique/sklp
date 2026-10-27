//go:build linux

package ociproc

import "testing"

func TestResourcesSpec_CPUQuotaPeriod(t *testing.T) {
	cases := []struct {
		cores      float64
		wantQuota  int64
		wantPeriod int64
		wantCPU    bool
	}{
		{cores: 0, wantCPU: false},
		{cores: -1, wantCPU: false},
		{cores: 2.0, wantQuota: 200000, wantPeriod: 100000, wantCPU: true},
		{cores: 1.0, wantQuota: 100000, wantPeriod: 100000, wantCPU: true},
		{cores: 0.5, wantQuota: 50000, wantPeriod: 100000, wantCPU: true},
		{cores: 1.5, wantQuota: 150000, wantPeriod: 100000, wantCPU: true},
	}
	for _, c := range cases {
		res, _ := ResourcesSpec(Limits{CPUCores: c.cores})
		cpu, has := res["cpu"].(map[string]any)
		if has != c.wantCPU {
			t.Errorf("cores=%v: cpu present=%v want %v", c.cores, has, c.wantCPU)
			continue
		}
		if !c.wantCPU {
			continue
		}
		if cpu["quota"].(int64) != c.wantQuota || cpu["period"].(int64) != c.wantPeriod {
			t.Errorf("cores=%v: %d/%d want %d/%d", c.cores,
				cpu["quota"], cpu["period"], c.wantQuota, c.wantPeriod)
		}
	}
}

func TestResourcesSpec_NoLimits(t *testing.T) {
	if res, ok := ResourcesSpec(Limits{}); ok || res != nil {
		t.Errorf("no limits = %v, %v; want nil, false", res, ok)
	}
}

func TestResourcesSpec_PerController(t *testing.T) {
	// memory only
	res, ok := ResourcesSpec(Limits{MemoryBytes: 512 << 20})
	if !ok || res["cpu"] != nil || res["pids"] != nil {
		t.Errorf("memory-only spec wrong: %v", res)
	}
	if res["memory"].(map[string]any)["limit"].(int64) != 512<<20 {
		t.Error("memory.limit mismatch")
	}
	// pids only
	res, _ = ResourcesSpec(Limits{PidsMax: 256})
	if res["cpu"] != nil || res["memory"] != nil {
		t.Errorf("pids-only spec wrong: %v", res)
	}
	if res["pids"].(map[string]any)["limit"].(int64) != 256 {
		t.Error("pids.limit mismatch")
	}
	// all three
	res, _ = ResourcesSpec(Limits{CPUCores: 1, MemoryBytes: 1 << 20, PidsMax: 128})
	for _, k := range []string{"cpu", "memory", "pids"} {
		if res[k] == nil {
			t.Errorf("missing %q when all limits set", k)
		}
	}
}

func TestLimitsAny(t *testing.T) {
	if (Limits{}).Any() {
		t.Error("empty Limits should be Any()=false")
	}
	for _, l := range []Limits{{CPUCores: 1}, {MemoryBytes: 1}, {PidsMax: 1}} {
		if !l.Any() {
			t.Errorf("%+v should be Any()=true", l)
		}
	}
}

func TestSystemdCgroupPath(t *testing.T) {
	if got := SystemdCgroupPath("sklp-ci"); got != "user.slice:sklp:sklp-ci" {
		t.Errorf("SystemdCgroupPath = %q, want user.slice:sklp:sklp-ci", got)
	}
}

func TestCgroupsPath(t *testing.T) {
	if got := CgroupsPath(CgroupSystemd, "sklp-ci"); got != "user.slice:sklp:sklp-ci" {
		t.Errorf("systemd path = %q", got)
	}
	if got := CgroupsPath(CgroupFS, "sklp-ci"); got != "/sklp/sklp-ci" {
		t.Errorf("cgroupfs path = %q", got)
	}
	if got := CgroupsPath(CgroupNone, "sklp-ci"); got != "" {
		t.Errorf("none path = %q", got)
	}
}

func TestMissingControllers(t *testing.T) {
	l := Limits{CPUCores: 1, MemoryBytes: 1, PidsMax: 1}
	if got := missingControllers("cpuset cpu io memory pids", l); len(got) != 0 {
		t.Errorf("all present, missing = %v", got)
	}
	got := missingControllers("memory pids", l)
	if len(got) != 1 || got[0] != "cpu" {
		t.Errorf("missing = %v, want [cpu]", got)
	}
	if got := missingControllers("", Limits{PidsMax: 1}); len(got) != 1 || got[0] != "pids" {
		t.Errorf("missing = %v, want [pids]", got)
	}
}
