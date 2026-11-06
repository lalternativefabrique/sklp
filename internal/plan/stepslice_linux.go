//go:build linux

package plan

import (
	"os/exec"
	"strconv"
	"sync"
)

var (
	stepSliceOnce       sync.Once
	stepSliceSystemdRun string
)

// stepSliceBudget is the cap of the shared step slice, or 0 when steps do not
// run under it: already inside a capped cgroup (a CI space), or no systemd
// user manager to own the slice.
func stepSliceBudget() int {
	stepSliceOnce.Do(func() {
		if cgroupCPUs() > 0 {
			return
		}
		run, err := exec.LookPath("systemd-run")
		if err != nil {
			return
		}
		quota := "CPUQuota=" + strconv.Itoa(stepSliceCPUs()*100) + "%"
		if exec.Command("systemctl", "--user", "set-property", "--runtime", stepSliceName, quota).Run() != nil {
			return
		}
		stepSliceSystemdRun = run
	})
	if stepSliceSystemdRun == "" {
		return 0
	}
	return stepSliceCPUs()
}

// wrapInStepSlice runs cmd inside a transient scope of the shared step slice.
// systemd-run --scope execs in place, so the pid, process group, pdeathsig
// and netns set on cmd still apply to the step.
func wrapInStepSlice(cmd *exec.Cmd) {
	if !reachesUserBus(cmd.Env) || stepSliceBudget() == 0 {
		return
	}
	cmd.Args = stepSliceArgv(stepSliceSystemdRun, append([]string{cmd.Path}, cmd.Args[1:]...))
	cmd.Path = stepSliceSystemdRun
}
