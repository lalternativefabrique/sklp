package devstack

import (
	"os"

	"github.com/lalternativefabrique/sklp/internal/limarelay"
)

// relayDevLima replays this `sklp dev` invocation inside the Lima VM (see
// limarelay). It only returns on failure; on Linux it reports why the
// embedded runtime is unavailable instead.
func relayDevLima(cfg DevConfig) error { return limarelay.RelaySelf(cfg.Repo) }

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
