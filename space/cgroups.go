package space

import (
	"fmt"

	"github.com/lalternativefabrique/sklp/space/ociproc"
)

// cgroups v2 resource limits for the embedded runtime. The pure mapping +
// rootless delegation check live in the shared ociproc leaf (so the build path
// reuses them without an import cycle); this file is the thin Config adapter for
// the run/space path.
//
// Config representation:
//   - CPULimit  float64: CPU cores (2.0 = two, 0.5 = half). 0 = unlimited.
//   - MemoryLimit int64: hard memory cap, bytes. 0 = unlimited.
//   - PidsLimit  int64: max processes/threads. 0 = unlimited (anti-fork-bomb).
//
// Rootless caveat: applying limits unprivileged needs cgroups v2 with a
// delegated subtree (systemd Delegate=yes). Present → set linux.cgroupsPath +
// --systemd-cgroup. Absent → Config.RequireLimits decides: false (dev) warns
// and runs unlimited; true (CI) FAILS, because silently dropping a cap lets one
// leaky run OOM the host.

// limits projects a Config onto the shared ociproc.Limits.
func (c Config) limits() ociproc.Limits {
	return ociproc.Limits{
		CPUCores:    c.CPULimit,
		MemoryBytes: c.MemoryLimit,
		PidsMax:     c.PidsLimit,
	}
}

// resourcesSpec builds the linux.resources map for the configured limits.
func resourcesSpec(cfg Config) (map[string]any, bool) {
	return ociproc.ResourcesSpec(cfg.limits())
}

// wantsLimits reports whether the config asks for any cgroup limit.
func (c Config) wantsLimits() bool {
	return c.limits().Any()
}

// systemdCgroupPath returns the linux.cgroupsPath in systemd slice form.
func systemdCgroupPath(id string) string {
	return ociproc.SystemdCgroupPath(id)
}

// serviceLimits is the outcome of reconciling a service's requested caps with
// what the host can actually enforce.
type serviceLimits struct {
	applied       ociproc.Limits
	cgroupsPath   string
	systemdCgroup bool
}

// resolveServiceLimits decides whether a deployed service's requested caps can
// be applied. Rootless cgroups v2 needs a delegated subtree; without it a cap
// is either dropped (best-effort) or fatal when the service asked for it to be
// a contract (RequireResources) — silently running unbounded lets one leaky
// service OOM the host and take every other space on the VM with it.
func resolveServiceLimits(svc DeployService, containerID string) (serviceLimits, error) {
	if !svc.Resources.Any() {
		return serviceLimits{}, nil
	}
	driver, reason := ociproc.ResolveCgroupDriver(svc.Resources)
	if driver == ociproc.CgroupNone {
		if svc.RequireResources {
			return serviceLimits{}, fmt.Errorf("%s: enable cgroups v2 delegation (systemd user@.service `Delegate=cpu memory pids`) or clear require", reason)
		}
		return serviceLimits{}, nil
	}
	return serviceLimits{
		applied:       svc.Resources,
		cgroupsPath:   ociproc.CgroupsPath(driver, containerID),
		systemdCgroup: driver == ociproc.CgroupSystemd,
	}, nil
}
