package space

import (
	"testing"

	"github.com/lalternativefabrique/sklp/space/ociproc"
)

func serviceSpecLinux(t *testing.T, svc Service) map[string]any {
	t.Helper()
	spec := buildServiceSpec("/rootfs", svc, nil, []string{"/bin/true"}, nil, "", 1000, 1000, nil)
	linux, ok := spec["linux"].(map[string]any)
	if !ok {
		t.Fatal("spec has no linux section")
	}
	return linux
}

func TestServiceSpecNoResourcesStaysUnbounded(t *testing.T) {
	linux := serviceSpecLinux(t, Service{Name: "api"})
	if _, ok := linux["resources"]; ok {
		t.Error("linux.resources set without a requested limit")
	}
	if _, ok := linux["cgroupsPath"]; ok {
		t.Error("linux.cgroupsPath set without a requested limit")
	}
}

func TestServiceSpecCarriesResources(t *testing.T) {
	svc := Service{
		Name:        "api",
		CgroupsPath: ociproc.SystemdCgroupPath("sklp-api-0"),
		Resources:   ociproc.Limits{CPUCores: 1.5, MemoryBytes: 512 << 20, PidsMax: 2048},
	}
	linux := serviceSpecLinux(t, svc)
	res, ok := linux["resources"].(map[string]any)
	if !ok {
		t.Fatal("linux.resources missing")
	}
	mem := res["memory"].(map[string]any)
	if mem["limit"].(int64) != 512<<20 {
		t.Errorf("memory.limit = %v", mem["limit"])
	}
	cpu := res["cpu"].(map[string]any)
	if cpu["quota"].(int64) != 150000 || cpu["period"].(int64) != 100000 {
		t.Errorf("cpu = %v", cpu)
	}
	pids := res["pids"].(map[string]any)
	if pids["limit"].(int64) != 2048 {
		t.Errorf("pids.limit = %v", pids["limit"])
	}
	if got := linux["cgroupsPath"]; got != ociproc.SystemdCgroupPath("sklp-api-0") {
		t.Errorf("cgroupsPath = %v", got)
	}
}

func TestServiceSpecResourcesWithoutCgroupsPath(t *testing.T) {
	svc := Service{Name: "api", Resources: ociproc.Limits{PidsMax: 4096}}
	linux := serviceSpecLinux(t, svc)
	if _, ok := linux["resources"]; !ok {
		t.Error("linux.resources missing")
	}
	if _, ok := linux["cgroupsPath"]; ok {
		t.Error("cgroupsPath set without a container id")
	}
}

func TestResolveServiceLimitsNoRequest(t *testing.T) {
	got, err := resolveServiceLimits(DeployService{Name: "api"}, "sklp-api-0")
	if err != nil {
		t.Fatalf("resolveServiceLimits: %v", err)
	}
	if got.applied.Any() || got.systemdCgroup || got.cgroupsPath != "" {
		t.Errorf("no request should resolve to the zero value, got %+v", got)
	}
}

func TestResolveServiceLimitsHonoursHostCapability(t *testing.T) {
	svc := DeployService{Name: "api", Resources: ociproc.Limits{MemoryBytes: 256 << 20}}
	driver, _ := ociproc.ResolveCgroupDriver(svc.Resources)
	delegated := driver != ociproc.CgroupNone

	got, err := resolveServiceLimits(svc, "sklp-api-0")
	if err != nil {
		t.Fatalf("best-effort must never fail: %v", err)
	}
	if delegated {
		wantPath := ociproc.CgroupsPath(driver, "sklp-api-0")
		if got.systemdCgroup != (driver == ociproc.CgroupSystemd) || got.cgroupsPath != wantPath || got.applied != svc.Resources {
			t.Errorf("delegated host should apply the caps, got %+v", got)
		}
	} else if got.applied.Any() || got.systemdCgroup {
		t.Errorf("undelegated host should drop the caps, got %+v", got)
	}

	svc.RequireResources = true
	_, err = resolveServiceLimits(svc, "sklp-api-0")
	if delegated && err != nil {
		t.Errorf("delegated host with require: unexpected error %v", err)
	}
	if !delegated && err == nil {
		t.Error("undelegated host with require: want a hard failure, got nil")
	}
}
