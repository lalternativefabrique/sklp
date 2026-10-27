package controller

import (
	"log"
	"sync"
	"time"

	"github.com/lalternativefabrique/sklp/spacemanager"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

// gatewayManager brings the host gateway singleton up for the spaces a
// controller runs. The gateway (traefik + cert store) is ONE per HOST, shared by
// every space — and every named space runs as its OWN detached supervisor
// process, so a ref count would have to span processes to be correct. It did
// not: an in-process count saw only its own space and tore the gateway down on
// the first space's teardown, cutting the front door out from under every other
// live space (in this AND other processes).
//
// So the gateway's lifecycle is decoupled from any single space: acquire brings
// it up idempotently (EnsureUp no-ops when the container is already running —
// the runc-state check is host-level, so it sees a gateway another process
// started), and teardown NEVER stops it. The gateway is a host singleton, torn
// down only by an explicit `sklp gateway down`. A space teardown still unroutes
// its own routes (controller.go), so a deleted space stops being fronted without
// touching its siblings.
//
// Keyed by driver name: in practice a host runs one driver, but two distinct
// drivers would each get their own singleton.
type gatewayManager struct {
	mu      sync.Mutex
	drivers map[string]spacemanager.GatewayDriver
}

func newGatewayManager() *gatewayManager {
	return &gatewayManager{drivers: map[string]spacemanager.GatewayDriver{}}
}

// acquire returns the shared gateway for driverName, bringing it up idempotently.
// EnsureUp is a no-op when the container is already running (host-level runc
// state), so a second process whose space also needs the gateway reuses the one
// already up. Memoised per process so repeated spaces in one controller share a
// handle.
func (m *gatewayManager) acquire(driverName string) (spacemanager.GatewayDriver, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if gw, ok := m.drivers[driverName]; ok {
		return gw, nil
	}
	gw, err := spacemanager.OpenGateway(driverName)
	if err != nil {
		return nil, err
	}
	if err := gw.EnsureUp(); err != nil {
		return nil, err
	}
	m.drivers[driverName] = gw
	go m.supervise(driverName, gw)
	// Expose the singleton on the node control API (RouteGateway/RouteGatewayUp)
	// so an operator pilots it remotely — `sklp gateway status|up -context <c>` —
	// instead of SSHing to the node.
	control.InstallGatewayControl(
		func() (control.GatewayControlStatus, error) {
			st, err := gw.Status()
			if err != nil {
				return control.GatewayControlStatus{}, err
			}
			return control.GatewayControlStatus{Driver: st.Driver, Running: st.Running, Listening: st.Listening}, nil
		},
		gw.EnsureUp,
	)
	return gw, nil
}

// superviseInterval paces the gateway singleton health check. 30s bounds the
// front-door outage after a traefik death; the check itself is a host-level
// runc-state read, cheap enough to run forever.
const superviseInterval = 30 * time.Second

// supervise re-ensures the host gateway singleton for the life of the process.
// The singleton was launched once at acquire and then belonged to nobody: a
// dead traefik took every HTTPS front down until an operator ran `sklp gateway
// up` by hand. EnsureUp is idempotent (no-op while the container is alive), so
// the loop is a watchdog, not a restarter — it only acts when Status reports
// the singleton down. One goroutine per driver, started on first acquire.
func (m *gatewayManager) supervise(driverName string, gw spacemanager.GatewayDriver) {
	for range time.Tick(superviseInterval) {
		st, err := gw.Status()
		if err == nil && st.Running {
			continue
		}
		log.Printf("gateway %s: singleton down (status: %v); re-ensuring", driverName, err)
		if uerr := gw.EnsureUp(); uerr != nil {
			log.Printf("gateway %s: EnsureUp failed: %v", driverName, uerr)
			continue
		}
		log.Printf("gateway %s: singleton relaunched", driverName)
	}
}
