package spacemanager

import "context"

// Launcher is the injected dependency that turns a desired Service into a
// running, detached OS process and returns the PID the agent watches via a
// pidfd. Decoupling it lets the reconciliation core be tested with plain
// processes (execLauncher) instead of real containers (runcLauncher), which
// would need privileges not always available in CI.
//
// Contract: Start launches the service detached (the caller does NOT wait on
// it — the subreaper does) and returns the init PID. Stop tears the running
// instance down idempotently; it is called on death-cleanup and teardown and
// must tolerate an already-gone instance. Addr reports the internal address
// the proxy targets for that instance.
//
// IDENTITY: from milestone 3 the agent runs N instances per service, so the
// id it passes to Start/Stop/Addr is the per-INSTANCE id (<service>-<i>), not
// the service name. svc.Name is still the service; the agent overrides it with
// the instance id before launching so the launcher keys its bookkeeping per
// instance.
type Launcher interface {
	// Start launches svc detached and returns its init PID. svc.Name is the
	// instance id.
	Start(ctx context.Context, svc Service) (pid int, err error)

	// Stop tears down the running instance by id. Best-effort and idempotent:
	// a no-op if nothing is running.
	Stop(id string)

	// Addr returns the internal address the proxy should route to for the
	// instance id (e.g. "10.0.0.4:8080" with runc, a synthetic string with the
	// exec launcher). Empty if the instance has no resolvable address yet.
	Addr(id string) string
}

// ipPortProvider is the optional launcher capability the agent type-asserts
// to resolve a service's real container IP and declared port: for
// GET /v1/state's snapshot. The runc launcher implements it via
// Space.ProbeCoords (already public, read-only); the exec test launcher does
// not, so Snapshot's IP/Port stay zero there. This exists so the Network
// Controller (spacemanager/netctrl) can POLL a service's reachability from
// the manifest + this snapshot, entirely independent of space.Space — Space
// itself has no knowledge that anything reads this.
type ipPortProvider interface {
	IPPort(name string) (ip string, port int, ok bool)
}
