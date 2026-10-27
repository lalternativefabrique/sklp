//go:build linux

package space

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lalternativefabrique/sklp/space/ociproc"
	"github.com/lalternativefabrique/sklp/space/spacenet"
)

// childCgroupSlice is the delegated slice a child-userns container's cgroup
// is created in. A var so tests can point it at a fixture tree.
var childCgroupSlice = func() string {
	return filepath.Join(spacenet.CgroupUserRoot(), "app.slice")
}

// clearChildCgroup empties and removes the cgroup applyChildLimits made for
// id before the service starts again. Processes still in it belong to a run
// the previous agent never tore down: the child root's runc state names them
// running, so the next runc run fails "container with given ID already
// exists" and the service never comes back. The operator owns this cgroup,
// so cgroup.kill ends them the way systemd stops a unit. Left in place, the
// empty cgroup would meet runc too: runc tolerates a cgroup it cannot
// create, not one that exists and refuses it.
func clearChildCgroup(id string) {
	dir := filepath.Join(childCgroupSlice(), id)
	if cgroupPopulated(dir) {
		_ = os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0o644)
		for i := 0; i < 50 && cgroupPopulated(dir); i++ {
			time.Sleep(100 * time.Millisecond)
		}
	}
	_ = os.Remove(dir)
}

func cgroupPopulated(dir string) bool {
	procs, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	return err == nil && len(strings.TrimSpace(string(procs))) > 0
}

// applyChildLimits places a running child-userns container in a cgroup of the
// operator's delegated subtree and sets its limits. runc cannot do it as the
// child root, but the operator owns app.slice and cgroup v2 lets the owner of
// the destination move any process into it. The cgroup is the one runc would
// have used (app.slice/<id>), so signalling and reaping find it as usual.
func applyChildLimits(id string, pid int, l ociproc.Limits) error {
	slice := childCgroupSlice()
	for _, c := range []string{"cpu", "memory", "pids"} {
		_ = os.WriteFile(filepath.Join(slice, "cgroup.subtree_control"), []byte("+"+c), 0o644)
	}
	dir := filepath.Join(slice, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("cgroup %s: %w", dir, err)
	}
	write := func(name, value string) error {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o644); err != nil {
			return fmt.Errorf("cgroup %s: %s=%s: %w", id, name, value, err)
		}
		return nil
	}
	if l.PidsMax > 0 {
		if err := write("pids.max", strconv.FormatInt(l.PidsMax, 10)); err != nil {
			return err
		}
	}
	if l.MemoryBytes > 0 {
		if err := write("memory.max", strconv.FormatInt(l.MemoryBytes, 10)); err != nil {
			return err
		}
	}
	if quota, period, ok := ociproc.CPUQuotaPeriod(l.CPUCores); ok {
		if err := write("cpu.max", fmt.Sprintf("%d %d", quota, period)); err != nil {
			return err
		}
	}
	return write("cgroup.procs", strconv.Itoa(pid))
}
