//go:build linux

package space

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/lalternativefabrique/sklp/space/spacenet"
)

// reapSpaceCgroups kills whatever still holds a cgroup under this space's
// container-id prefix and removes the emptied cgroup dirs. It is the teardown of
// last resort: it needs neither runc state nor a space directory, so it still
// works once those are gone — the exact state in which a leaked service is
// otherwise unreachable and blocks the next run with "container's cgroup is not
// empty". A var so tests can stub it.
var reapSpaceCgroups = func(prefix string) {
	spacenet.ReapCgroupsByPrefix(prefix, nil, false,
		func(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) },
		os.Remove)
}

// leakedCgroupHint turns runc's "container's cgroup is not empty" — which names
// neither the offending processes nor any way out — into an actionable line. It
// returns "" for every other error.
func leakedCgroupHint(err error, containerID string) string {
	if err == nil || !strings.Contains(err.Error(), "cgroup is not empty") {
		return ""
	}
	pids := spacenet.CgroupProcsFor(containerID)
	if len(pids) == 0 {
		return "hint: a prior run left this container's cgroup behind; `sklp clean` clears it"
	}
	strs := make([]string, 0, len(pids))
	for _, p := range pids {
		strs = append(strs, strconv.Itoa(p))
	}
	return fmt.Sprintf("hint: cgroup %s is still held by pid(s) %s from an earlier run; `sklp clean` reaps them",
		containerID, strings.Join(strs, ", "))
}
