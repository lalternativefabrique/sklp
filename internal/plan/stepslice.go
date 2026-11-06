package plan

import (
	"os"
	"runtime"
	"strconv"
	"strings"
)

const stepSliceName = "sklp-steps.slice"

const minStepSliceCPUs = 2

// stepSliceCPUs is the shared cap every host-run step of every concurrent
// sklp run lives under: SKLP_CPUS, else half the host.
func stepSliceCPUs() int {
	if n, err := strconv.Atoi(os.Getenv(cpuBudgetEnv)); err == nil && n > 0 {
		return n
	}
	return halfHost(runtime.NumCPU())
}

func halfHost(hostCPUs int) int {
	n := hostCPUs / 2
	if n < minStepSliceCPUs {
		n = minStepSliceCPUs
	}
	if n > hostCPUs {
		n = hostCPUs
	}
	return n
}

func reachesUserBus(env []string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, "XDG_RUNTIME_DIR=") || strings.HasPrefix(kv, "DBUS_SESSION_BUS_ADDRESS=") {
			return true
		}
	}
	return false
}

func stepSliceArgv(systemdRun string, argv []string) []string {
	return append([]string{systemdRun, "--user", "--scope", "--quiet", "--collect", "--slice=" + stepSliceName, "--"}, argv...)
}
