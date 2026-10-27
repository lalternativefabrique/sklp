package deployer

import (
	"context"
	"fmt"
	"io"

	"github.com/lalternativefabrique/sklp/spacemanager"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

// RemoteApplyConfig is the typed input for a REMOTE deploy: resolve the local
// deploy.yaml exactly like Up does, then reconcile it against a supervisor
// reached over the network (control.Client built from DialNet) instead of the
// local socket. This is the `kubectl apply` path — the manifest is read locally
// and POSTed; nothing is copied to the server.
type RemoteApplyConfig struct {
	Stack string // stack name stamped onto meta.Stack (ownership tag)
	File  string // absolute path to the resolved deploy.yaml (LOCAL)
	Space string // named/ephemeral space to reconcile
	Repo  string // repository root the file is relative to (informational)
	// Force adopts a space already owned by a DIFFERENT stack instead of
	// letting the server's cross-stack ownership guard refuse the apply.
	Force bool
}

// RemoteApply resolves the local manifest and POSTs it to an already-running
// remote supervisor via client.Apply, printing the kubectl-style outcome to out.
// It does NOT start a supervisor: a remote deploy targets a space that is already
// up and listening (`sklp deploy --listen` on the server). ErrNoSupervisor from
// the client surfaces as an actionable error. The manifest resolution mirrors
// Up (Load → Resolve → injectIngress) so local and remote deploys agree on the
// desired state.
func RemoteApply(ctx context.Context, client *control.Client, cfg RemoteApplyConfig, out io.Writer) error {
	dcfg, err := spacemanager.Load(cfg.File)
	if err != nil {
		return err
	}
	// The gateway mode is a NODE-level property the client cannot know (and must
	// not claim to know): only the remote agent's own `--gateway-mode` is
	// authoritative. Resolve with "" — the server overwrites spec.Meta.GatewayMode
	// with its own resolved value before applying (see handleApply), so this is
	// never trusted, only used here for the (best-effort, non-per-space-gated)
	// local shaping of the desired set.
	services, meta, err := dcfg.Resolve(cfg.Space, "")
	if err != nil {
		return err
	}
	meta.Stack = cfg.Stack
	warnLiteralSecrets(dcfg, out)

	// Mirror Up's ingress injection so the remote desired state includes the
	// per-space ingress service when the driver + routes call for it.
	services, err = injectIngress(dcfg.GatewayDriverName(), services, meta)
	if err != nil {
		return err
	}

	// Resolve each registry password from the OPERATOR's environment (the local
	// .env) and ship the value in the spec: the detached agent can't read the
	// operator's env, so os.Getenv there is empty and a private pull fails with
	// insufficient_scope. The value travels only over the TLS control API and is
	// stripped before the supervisor persists the manifest (persistManifest), so
	// the on-disk manifest stays name-only. Service env values travel resolved
	// the same way and are stripped by the same path (StripEnvRefs).
	meta.Registries = spacemanager.ResolveRegistryPasswords(meta.Registries)

	spec, err := control.MarshalApplySpec(services, meta)
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	apply := client.Apply
	if cfg.Force {
		apply = client.ApplyForce
	}
	res, err := apply(ctx, spec)
	if err != nil {
		return fmt.Errorf("apply to remote supervisor for space %q: %w", meta.Name, err)
	}
	printRemoteApplyResult(out, meta.Name, res)
	return nil
}

// printRemoteApplyResult renders the kubectl-style apply outcome. Kept local to
// the use case (the CLI's printApplyResult is package main) so RemoteApply needs
// no callback for its human output.
func printRemoteApplyResult(out io.Writer, space string, res control.ApplyResult) {
	switch res.Result {
	case control.ResultUnchanged:
		fmt.Fprintf(out, "space %q: unchanged\n", space)
	default: // configured / created / rejected
		fmt.Fprintf(out, "space %q: %s (%d change(s))\n", space, res.Result, len(res.Changes))
		for _, c := range res.Changes {
			fmt.Fprintf(out, "  - %s\n", c)
		}
	}
	// Printed for every outcome, "unchanged" included: a re-apply of a manifest
	// whose database nothing backs up should still say so.
	for _, w := range res.Warnings {
		fmt.Fprintf(out, "  ! %s\n", w)
	}
}
