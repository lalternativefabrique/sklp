//go:build linux

package ociproc

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// cgroups v2 resource limits, shared by the run/space path (package runtime)
// and the build path (RuncStepRunner). Extracted here so both emit the SAME
// linux.resources mapping and use the SAME rootless delegation check — a build
// step and a service must be bounded identically.
//
// Mapping to the runtime-spec linux.resources section:
//   - memory.limit = MemoryBytes; runc writes it to memory.max.
//   - cpu.quota/period: cgroups v2 cpu.max = "<quota> <period>"; period fixed
//     at 100ms, quota = round(CPUCores * period).
//   - pids.limit = PidsMax; runc writes it to pids.max. The cheap
//     anti-fork-bomb knob — the pids controller is delegated even when
//     cpu/memory are not, so it applies where the others can't.

// cpuPeriodMicros is the fixed cgroups v2 accounting period (100ms).
const cpuPeriodMicros = 100000

// Limits is a resource request, independent of any caller Config. Zero in a
// field means "no limit for that controller".
type Limits struct {
	CPUCores    float64 // 2.0 = two cores, 0.5 = half a core
	MemoryBytes int64   // hard cap, bytes
	PidsMax     int64   // max processes/threads
}

// Any reports whether any limit is requested.
func (l Limits) Any() bool {
	return l.CPUCores > 0 || l.MemoryBytes > 0 || l.PidsMax > 0
}

// cpuQuotaPeriod converts a core count into the cgroups v2 quota/period pair.
// cores<=0 means no CPU limit (ok=false).
// CPUQuotaPeriod is cpuQuotaPeriod for callers that write cpu.max themselves.
func CPUQuotaPeriod(cores float64) (quota, period int64, ok bool) { return cpuQuotaPeriod(cores) }

func cpuQuotaPeriod(cores float64) (quota, period int64, ok bool) {
	if cores <= 0 {
		return 0, 0, false
	}
	period = cpuPeriodMicros
	quota = int64(cores*float64(period) + 0.5)
	if quota < 1 {
		quota = 1
	}
	return quota, period, true
}

// ResourcesSpec builds the linux.resources map for l. Returns ok=false when no
// limit is requested, so a caller can leave the spec byte-identical to the
// no-limit behaviour.
func ResourcesSpec(l Limits) (map[string]any, bool) {
	res := map[string]any{}
	if l.MemoryBytes > 0 {
		res["memory"] = map[string]any{"limit": l.MemoryBytes}
	}
	if quota, period, ok := cpuQuotaPeriod(l.CPUCores); ok {
		res["cpu"] = map[string]any{"quota": quota, "period": period}
	}
	if l.PidsMax > 0 {
		res["pids"] = map[string]any{"limit": l.PidsMax}
	}
	if len(res) == 0 {
		return nil, false
	}
	return res, true
}

// CgroupDriver is how runc places a bounded container in the cgroup tree.
type CgroupDriver int

const (
	// CgroupNone means the requested limits cannot be applied on this host.
	CgroupNone CgroupDriver = iota
	// CgroupSystemd runs runc with --systemd-cgroup under the operator's
	// delegated user slice — the rootless path.
	CgroupSystemd
	// CgroupFS writes cgroupfs directly under /sys/fs/cgroup/sklp — root
	// without systemd (a VM guest, a CI container), where there is nothing to
	// delegate from and nothing stops the process from owning a subtree.
	CgroupFS
)

const cgroupFSRoot = "/sys/fs/cgroup/sklp"

// ResolveCgroupDriver picks the driver able to apply the limits l requests,
// or CgroupNone with a human-readable reason. Rootless needs cgroups v2 with
// a systemd-delegated subtree exposing every controller a requested limit
// needs; checking only the requested controllers means a pids-only limit
// still applies on a host that delegates pids but not cpu/memory — the common
// rootless case. Root without systemd owns the tree and enables the
// controllers itself.
func ResolveCgroupDriver(l Limits) (CgroupDriver, string) {
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		return CgroupNone, "cgroups v2 unified hierarchy not mounted at /sys/fs/cgroup"
	}
	if !systemdRunning() {
		if os.Geteuid() != 0 {
			return CgroupNone, "systemd is not running, so runc has no cgroup manager for the delegated subtree (uid 0 inside a container)"
		}
		if reason := ensureCgroupFSRoot(l); reason != "" {
			return CgroupNone, reason
		}
		return CgroupFS, ""
	}
	rel, err := selfCgroupV2Path()
	if err != nil {
		return CgroupNone, err.Error()
	}
	ctrlPath := filepath.Join("/sys/fs/cgroup", rel, "cgroup.controllers")
	data, err := os.ReadFile(ctrlPath)
	if err != nil {
		return CgroupNone, fmt.Sprintf("cannot read delegated controllers at %s: %v", ctrlPath, err)
	}
	if missing := missingControllers(string(data), l); len(missing) > 0 {
		return CgroupNone, fmt.Sprintf("delegated cgroup %q lacks the %s controller(s) (have %s)",
			rel, strings.Join(missing, "+"), strings.Join(strings.Fields(string(data)), " "))
	}
	return CgroupSystemd, ""
}

// CgroupsV2Delegated reports whether the current process can apply the limits
// l requests, with a reason when it cannot. See ResolveCgroupDriver.
func CgroupsV2Delegated(l Limits) (bool, string) {
	d, reason := ResolveCgroupDriver(l)
	return d != CgroupNone, reason
}

// CgroupsPath returns the runtime-spec linux.cgroupsPath for container id
// under driver d: the systemd slice form, or an absolute cgroupfs path.
func CgroupsPath(d CgroupDriver, id string) string {
	switch d {
	case CgroupSystemd:
		return SystemdCgroupPath(id)
	case CgroupFS:
		return cgroupFSRoot[len("/sys/fs/cgroup"):] + "/" + id
	}
	return ""
}

// neededControllers lists the cgroup v2 controllers the limits l require.
func neededControllers(l Limits) []string {
	var want []string
	if l.CPUCores > 0 {
		want = append(want, "cpu")
	}
	if l.MemoryBytes > 0 {
		want = append(want, "memory")
	}
	if l.PidsMax > 0 {
		want = append(want, "pids")
	}
	return want
}

// missingControllers returns the controllers l needs that a cgroup.controllers
// file content does not list.
func missingControllers(controllers string, l Limits) []string {
	have := map[string]bool{}
	for _, c := range strings.Fields(controllers) {
		have[c] = true
	}
	var missing []string
	for _, c := range neededControllers(l) {
		if !have[c] {
			missing = append(missing, c)
		}
	}
	return missing
}

// ensureCgroupFSRoot creates /sys/fs/cgroup/sklp and enables the controllers l
// needs down to it, so runc can write cpu.max & co in /sklp/<id>. The root
// cgroup is exempt from the no-internal-process rule, so enabling controllers
// there is always allowed; each is enabled on its own because a single write
// naming an unavailable controller is rejected as a whole. Returns a reason
// when a needed controller still is not available. Idempotent.
func ensureCgroupFSRoot(l Limits) string {
	if err := os.MkdirAll(cgroupFSRoot, 0o755); err != nil {
		return fmt.Sprintf("cgroupfs: create %s: %v", cgroupFSRoot, err)
	}
	for _, dir := range []string{"/sys/fs/cgroup", cgroupFSRoot} {
		for _, c := range neededControllers(l) {
			_ = os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"), []byte("+"+c), 0o644)
		}
	}
	data, err := os.ReadFile(filepath.Join(cgroupFSRoot, "cgroup.subtree_control"))
	if err != nil {
		return fmt.Sprintf("cgroupfs: read %s/cgroup.subtree_control: %v", cgroupFSRoot, err)
	}
	if missing := missingControllers(string(data), l); len(missing) > 0 {
		return fmt.Sprintf("cgroupfs: %s cannot enable the %s controller(s) (have %s)",
			cgroupFSRoot, strings.Join(missing, "+"), strings.Join(strings.Fields(string(data)), " "))
	}
	return ""
}

// systemdRunning mirrors sd_booted(3): runc's --systemd-cgroup manager needs a
// live systemd, which root inside a container does not have.
func systemdRunning() bool {
	fi, err := os.Stat("/run/systemd/system")
	return err == nil && fi.IsDir()
}

// selfCgroupV2Path returns the current process' cgroup v2 path relative to the
// unified mount (without the leading slash).
func selfCgroupV2Path() (string, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", fmt.Errorf("read /proc/self/cgroup: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "0::") {
			return strings.TrimPrefix(strings.TrimPrefix(line, "0::"), "/"), nil
		}
	}
	return "", fmt.Errorf("no cgroup v2 (0::) entry in /proc/self/cgroup")
}

// SystemdCgroupPath returns the runtime-spec linux.cgroupsPath value in the
// systemd "slice:prefix:name" form that runc consumes with --systemd-cgroup.
// The slice is the operator's user slice so the limited unit lands inside the
// delegated subtree.
func SystemdCgroupPath(id string) string {
	return fmt.Sprintf("user.slice:sklp:%s", id)
}
