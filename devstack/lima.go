package devstack

import (
	"os"

	"github.com/lalternativefabrique/sklp/internal/limarelay"
	"github.com/lalternativefabrique/sklp/space/muxport"
)

// relayDevLima replays this `sklp dev` invocation inside the Lima VM (see
// limarelay). It only returns on failure; on Linux it reports why the
// embedded runtime is unavailable instead.
func relayDevLima(cfg DevConfig, spaceName string) error {
	release := limarelay.ForwardHostPorts([]int{muxport.For(spaceName)})
	defer release()
	return limarelay.RelaySelf(cfg.Repo)
}

// relayToRunningVM relays this invocation into the VM when one is running:
// the stacks ran in the guest, so their statefiles and logs live there. A
// false return lets the host-side verb proceed.
func relayToRunningVM(_ []string) bool {
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
