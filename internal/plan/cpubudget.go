package plan

import (
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// cpuBudgetEnv is the CPU budget the spawner hands the runner. The cgroup cap
// it mirrors is best-effort (no systemd → no delegation → dropped), so the env
// is the one signal that reaches every host.
const cpuBudgetEnv = "SKLP_CPUS"

// cpuBudget is the number of cores this run may use: the spawner's budget,
// else the cgroup cpu.max it runs under, else every core the kernel shows.
// runtime.NumCPU ignores cgroups, so on its own it lets three concurrent
// runs each schedule NumCPU steps × NumCPU compiler threads.
func cpuBudget() int {
	if n, err := strconv.Atoi(os.Getenv(cpuBudgetEnv)); err == nil && n > 0 {
		return n
	}
	if n := cgroupCPUs(); n > 0 {
		return n
	}
	return runtime.NumCPU()
}

// parseCPUMax turns a cgroups v2 cpu.max line ("<quota> <period>" or
// "max <period>") into whole cores, rounded up so a half-core cap still
// yields one job. 0 means unlimited or unreadable.
func parseCPUMax(s string) int {
	fields := strings.Fields(s)
	if len(fields) != 2 || fields[0] == "max" {
		return 0
	}
	quota, err1 := strconv.ParseFloat(fields[0], 64)
	period, err2 := strconv.ParseFloat(fields[1], 64)
	if err1 != nil || err2 != nil || quota <= 0 || period <= 0 {
		return 0
	}
	return int(math.Ceil(quota / period))
}

// withGOMAXPROCS bounds Go toolchain parallelism (go build/test -p defaults
// to GOMAXPROCS) to the run's budget unless the step set its own.
func withGOMAXPROCS(env map[string]string, cpus int) map[string]string {
	if _, ok := env["GOMAXPROCS"]; ok || cpus <= 0 {
		return env
	}
	out := make(map[string]string, len(env)+1)
	for k, v := range env {
		out[k] = v
	}
	out["GOMAXPROCS"] = strconv.Itoa(cpus)
	return out
}
