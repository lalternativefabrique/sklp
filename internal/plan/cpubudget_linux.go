//go:build linux

package plan

import (
	"os"
	"path/filepath"
	"strings"
)

// cgroupCPUs returns the tightest cpu.max cap on the current process' cgroup
// v2 path and its ancestors, or 0 when none applies.
func cgroupCPUs() int {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return 0
	}
	rel := ""
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "0::") {
			rel = strings.TrimPrefix(strings.TrimPrefix(line, "0::"), "/")
			break
		}
	}
	best := 0
	for dir := filepath.Join("/sys/fs/cgroup", rel); strings.HasPrefix(dir, "/sys/fs/cgroup"); dir = filepath.Dir(dir) {
		raw, err := os.ReadFile(filepath.Join(dir, "cpu.max"))
		if err == nil {
			if n := parseCPUMax(string(raw)); n > 0 && (best == 0 || n < best) {
				best = n
			}
		}
		if dir == "/sys/fs/cgroup" {
			break
		}
	}
	return best
}
