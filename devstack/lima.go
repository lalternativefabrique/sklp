package devstack

import (
	"os"

	"github.com/lalternativefabrique/sklp/internal/limarelay"
)

// relayDevLima replays this `sklp dev` invocation inside the Lima VM (see
// limarelay). It only returns on failure; on Linux it reports why the
// embedded runtime is unavailable instead.
func relayDevLima(cfg DevConfig) error {
	release := limarelay.ForwardHostPorts(declaredPorts(cfg.TaskPath))
	defer release()
	return limarelay.RelaySelf(cfg.Repo)
}

// declaredPorts lists the ports the stack's services bind, so the host can
// publish them for the run. A stack that does not parse declares none; the
// relayed run reports the parse error itself.
func declaredPorts(taskPath string) []int {
	st, err := Load(taskPath)
	if err != nil {
		return nil
	}
	var ports []int
	for _, svc := range st.Services {
		if svc.Def.Port > 0 {
			ports = append(ports, svc.Def.Port)
		}
	}
	return ports
}

// relayDownLima relays `sklp dev down` into the VM when one is running —
// the up ran in the guest, so its statefile lives there. A false return
// lets the host-side down proceed.
func relayDownLima(_ []string) bool {
	if !limarelay.VMRunning() {
		return false
	}
	cwd, err := os.Getwd()
	if err != nil {
		return false
	}
	_ = limarelay.RelaySelf(cwd)
	return false
}
