package spacemanager

import (
	"context"

	"github.com/lalternativefabrique/sklp/space"
)

// readyChecker is the injected readiness gate, mirroring the Launcher pattern:
// the production impl wraps space.WaitServiceReady (real TCP/HTTP/file/log
// probes), tests inject a fake that succeeds/fails/blocks on demand without any
// network or container. WaitReady blocks until the service named `name` is
// ready, the probe fails/times out (error), or ctx is cancelled (the agent
// cancels it when the init dies before readiness).
type readyChecker interface {
	WaitReady(ctx context.Context, name string, r *Probe) error
	CheckReady(ctx context.Context, name string, r *Probe) error
}

// runcReadyChecker is the production readiness gate. It resolves each service's
// probe coordinates from the runc launcher and delegates to the exported
// space.WaitServiceReady, reusing the exact production probe backend (no
// probe logic duplicated here).
type runcReadyChecker struct {
	rt *space.Space
}

func (c runcReadyChecker) WaitReady(ctx context.Context, name string, r *Probe) error {
	return space.WaitServiceReady(ctx, name, r, c.rt.ProbeCoords(name))
}

func (c runcReadyChecker) CheckReady(ctx context.Context, name string, r *Probe) error {
	return space.CheckServiceReady(ctx, name, r, c.rt.ProbeCoords(name))
}
