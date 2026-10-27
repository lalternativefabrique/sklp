package sklet

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lalternativefabrique/sklp/spacemanager"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

// TryReapply routes a fresh apply to an already-running supervisor (re-apply)
// rather than starting a new one. It returns applied=true when a live supervisor
// was found and reconciled (the caller then exits); applied=false when none is
// running (the caller starts a supervisor).
//
// The decision is isolated here so it is unit-testable without launching real
// containers: a present, answering control socket => re-apply; ErrNoSupervisor
// => start. Any other dial/apply error is surfaced (do NOT silently start a 2nd
// supervisor on an ambiguous failure).
func TryReapply(ctx context.Context, space string, services []spacemanager.Service, meta spacemanager.SpaceMeta, force bool) (applied bool, err error) {
	client, derr := control.Dial(space)
	if derr != nil {
		return false, nil // cannot even resolve the socket; fall through to start
	}
	hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, herr := client.Health(hctx); herr != nil {
		if errors.Is(herr, control.ErrNoSupervisor) {
			return false, nil // nobody home: start a supervisor
		}
		return false, fmt.Errorf("probing live supervisor for space %q: %w", space, herr)
	}

	// Cross-stack ownership guard: a live space is OWNED by the stack that last
	// deployed it (recorded in the persisted manifest). Re-applying a different
	// stack's manifest would reconcile away the owner's services — a silent,
	// destructive overwrite. Refuse unless --force adopts the space. The check is
	// best-effort by design: a missing/legacy manifest (empty Stack) or an
	// unreadable one is treated as "unknown owner" and adopts, so the guard never
	// blocks a legitimate redeploy of a pre-existing space.
	if !force {
		if owner, ok := LiveSpaceOwner(space); ok && owner != meta.Stack {
			return false, fmt.Errorf(
				"space %q is already deployed by stack %q; deploying stack %q would reconcile away its services.\n"+
					"  re-run with --force to adopt the space, or `sklp deploy delete --space %s` first",
				space, owner, meta.Stack, space)
		}
	}

	// A supervisor answered: reconcile it toward the local manifest. force is
	// passed through to the server-side guard too (handleApply has its own,
	// independent cross-stack check — force must clear both, or a caller that
	// passed --force here would still get refused server-side).
	spec, merr := control.MarshalApplySpec(services, meta)
	if merr != nil {
		return false, fmt.Errorf("encode manifest: %w", merr)
	}
	apply := client.Apply
	if force {
		apply = client.ApplyForce
	}
	res, aerr := apply(ctx, spec)
	if aerr != nil {
		return false, fmt.Errorf("apply to live supervisor for space %q: %w", space, aerr)
	}
	PrintApplyResult(space, res)
	return true, nil
}

// LiveSpaceOwner returns the stack that owns the live space, read from the
// persisted manifest (the prior deploy wrote it before launching). ok=false when
// no manifest exists, it cannot be read, or it predates the Stack field (empty):
// the caller treats every !ok as "unknown owner" and adopts, so the cross-stack
// guard never fires on a legacy or unreadable space.
func LiveSpaceOwner(space string) (stack string, ok bool) {
	_, meta, err := control.LoadManifest(space)
	if err != nil || meta.Stack == "" {
		return "", false
	}
	return meta.Stack, true
}
