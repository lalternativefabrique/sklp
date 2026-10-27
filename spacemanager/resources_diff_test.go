package spacemanager

import "testing"

// A changed cap must roll the container. runc reads linux.resources once, at
// create, so an apply that reports "configured" without a relaunch would leave
// the old cap in force while claiming the new one applied.
func TestSpecChanged_ResourcesTriggerRelaunch(t *testing.T) {
	base := Service{Name: "core", Image: "img:1"}
	base.Resources = ServiceResources{CPUCores: 1, MemoryBytes: 512 << 20, PidsMax: 200}

	for _, tc := range []struct {
		name string
		res  ServiceResources
	}{
		{"cpu raised", ServiceResources{CPUCores: 2, MemoryBytes: 512 << 20, PidsMax: 200}},
		{"memory lowered", ServiceResources{CPUCores: 1, MemoryBytes: 256 << 20, PidsMax: 200}},
		{"pids changed", ServiceResources{CPUCores: 1, MemoryBytes: 512 << 20, PidsMax: 100}},
		{"caps dropped", ServiceResources{}},
	} {
		next := base
		next.Resources = tc.res
		if !specChanged(base, next) {
			t.Errorf("%s: specChanged = false, so the service would keep its old cap", tc.name)
		}
		if !serviceDiffers(base, next) {
			t.Errorf("%s: serviceDiffers = false, so the apply would report unchanged", tc.name)
		}
	}
}

// Identical resources must NOT roll the container: a re-apply of the same
// manifest is expected to be a no-op.
func TestSpecChanged_IdenticalResourcesAreUnchanged(t *testing.T) {
	a := Service{Name: "core", Image: "img:1"}
	a.Resources = ServiceResources{CPUCores: 1, MemoryBytes: 512 << 20, PidsMax: 200}
	b := a
	if specChanged(a, b) {
		t.Error("an unchanged manifest must not roll the service")
	}
}

// A changed volumes list must roll the container. Mounts land in the OCI
// spec, which runc reads once at create, so an apply that reports
// "unchanged" would leave the old (or missing) mount in force while
// claiming the new one applied.
func TestSpecChanged_VolumesTriggerRelaunch(t *testing.T) {
	base := Service{Name: "core", Image: "img:1", Volumes: []string{"data:/var/lib/data"}}

	next := base
	next.Volumes = []string{"data:/var/lib/data", "cache:/var/lib/cache"}
	if !specChanged(base, next) {
		t.Error("added volume: specChanged = false, so the service would keep running without the new mount")
	}
	if !serviceDiffers(base, next) {
		t.Error("added volume: serviceDiffers = false, so the apply would report unchanged")
	}

	next = base
	next.Volumes = []string{"data:/var/lib/data:ro"}
	if !specChanged(base, next) {
		t.Error("changed mount mode: specChanged = false, so the service would keep its old mount")
	}
}
