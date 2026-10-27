package deployer

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/lalternativefabrique/sklp/spacemanager"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

// Hooks injects the supervisor-bound steps that physically live in the CLI
// package (they are shared with the `__supervise` daemon, so they cannot move
// here without dragging that whole surface). The use case owns the control flow
// + the os.Exit→error mapping; the hooks own the launch. All hooks return an
// error instead of calling os.Exit.
type Hooks struct {
	// TryReapply reconciles an already-running supervisor; applied=true means done.
	// force adopts a space owned by a different stack instead of refusing.
	TryReapply func(ctx context.Context, space string, services []spacemanager.Service, meta spacemanager.SpaceMeta, force bool) (applied bool, err error)
	// Launch starts a fresh supervisor (detached / daemonless / foreground).
	Launch func(cfg *spacemanager.DeployConfig, services []spacemanager.Service, meta spacemanager.SpaceMeta, listen string, out, errOut writer) error
}

// writer aliases io.Writer for the hook signatures (kept local for brevity).
type writer = interface {
	Write(p []byte) (int, error)
}

// hooks is set once by the CLI adapter (init in deploy_embedded.go).
var hooks Hooks

// SetHooks wires the CLI-resident supervisor steps. Called once at startup.
func SetHooks(h Hooks) { hooks = h }

// muxListenAddr is the host address the deploy-ensured gateway mux binds. Fixed
// default (matching `sklp gateway mux`'s -listen default); overridable via env
// for a host that must move the front door off :9080.
func muxListenAddr() string { return control.MuxListenAddr() }

// Up performs the LOCAL deploy described by cfg. No os.Exit; returns the error
// the caller maps to an exit code. Honors ctx.
func Up(ctx context.Context, cfg UpConfig) error {
	out := writer(cfg.Stdout)
	if cfg.Stdout == nil {
		out = os.Stdout
	}
	errOut := writer(cfg.Stderr)
	if cfg.Stderr == nil {
		errOut = os.Stderr
	}

	dcfg, err := spacemanager.Load(cfg.File)
	if err != nil {
		return err
	}

	services, meta, err := dcfg.Resolve(cfg.Space, cfg.GatewayMode)
	if err != nil {
		return err
	}
	// Stamp the owning stack onto the meta so it persists in the manifest and the
	// cross-stack guard in TryReapply can compare ownership. Resolve is pure
	// (config -> services + meta) and never sees the stack name; the use case
	// does, so it stamps here — before SaveManifest and before TryReapply.
	meta.Stack = cfg.Stack

	// Inject the per-space ingress service when the gateway driver supports it and
	// the space declares routes. Additive: a host-singleton driver (or a space
	// with no routes) leaves the set unchanged. Done BEFORE SaveManifest so the
	// ingress is part of the persisted desired state.
	services, err = injectIngress(dcfg.GatewayDriverName(), services, meta)
	if err != nil {
		return err
	}

	warnLiteralSecrets(dcfg, errOut)
	for i := range services {
		services[i].DeclaredImage = services[i].Image
	}

	// Persist the resolved manifest BEFORE anything else so a reboot reflects the
	// latest desired state even if the cwd repo later disappears. Atomic write.
	// Env values that came from a ${VAR} are persisted by NAME only; `services`
	// keeps its resolved values for the launch below.
	if err := control.SaveManifest(meta.Name, spacemanager.StripEnvRefs(services, meta.EnvRefs), meta); err != nil {
		return err
	}

	// Persist the network-listen address next to the manifest (no-op on empty).
	if err := control.SaveListenAddr(meta.Name, cfg.Listen); err != nil {
		fmt.Fprintf(errOut, "sklp deploy: warning: could not persist --listen address: %v\n", err)
	}

	// Establish the GLOBAL control identity EAGERLY, in this parent process at
	// deploy time, BEFORE the supervisor is launched (let alone a future `down`).
	// This is what makes the fingerprint/token stable across down+redeploy: the
	// detached supervisor's startNetListener would otherwise be the first to
	// ensure them, but by then a prior `down` has already deleted the legacy
	// spaces/<space>/ cert it must promote — so it would mint a fresh one. Doing
	// it here promotes the legacy per-space identity into the global control dir
	// on the first --listen deploy (while spaces/<space>/ still exists), after
	// which RemoveSpaceArtifacts (spaces/<space>/ only) can never touch it again.
	// Gated on --listen: a socket-only deploy has no network identity to mint.
	if cfg.Listen != "" {
		if _, _, err := control.EnsureServerCert(meta.Name); err != nil {
			return fmt.Errorf("establish control TLS identity for space %q: %w", meta.Name, err)
		}
		if _, err := control.EnsureToken(meta.Name); err != nil {
			return fmt.Errorf("establish control token for space %q: %w", meta.Name, err)
		}
	}

	// Persist the gateway driver so a reboot reconstructs the SAME front door.
	if err := control.SaveGatewayDriver(meta.Name, dcfg.GatewayDriverName()); err != nil {
		fmt.Fprintf(errOut, "sklp deploy: warning: could not persist gateway driver: %v\n", err)
	}

	// Per-space mode: GUARANTEE the host mux front door. The legacy singleton was
	// brought up by the controller; in per-space mode the front door is the mux,
	// so a deploy must ensure it exists (one host-wide systemd unit, idempotent —
	// a second per-space deploy reuses the running mux). Best-effort: a mux launch
	// failure leaves the space reachable only in-netns, surfaced as a warning.
	if meta.GatewayMode == spacemanager.GatewayModePerSpace && len(meta.Routes) > 0 {
		if selfExe, eerr := os.Executable(); eerr != nil {
			fmt.Fprintf(errOut, "sklp deploy: warning: could not resolve binary for the mux: %v\n", eerr)
		} else if merr := control.EnsureMuxUnit(selfExe, muxListenAddr()); merr != nil {
			fmt.Fprintf(errOut, "sklp deploy: warning: could not ensure the gateway mux: %v\n", merr)
		}
	}

	// Live supervisor already up? Re-apply (kubectl-style) and exit.
	if applied, err := hooks.TryReapply(ctx, meta.Name, services, meta, cfg.Force); err != nil {
		return err
	} else if applied {
		return nil
	}

	// No live supervisor: start one (detached / daemonless / foreground).
	return hooks.Launch(dcfg, services, meta, cfg.Listen, out, errOut)
}

// warnLiteralSecrets tells the operator which sensitive-looking env values were
// inlined in the deploy file. Those cannot be persisted by name, so they reach
// the on-disk manifest in cleartext; writing them as ${VAR} keeps them out.
func warnLiteralSecrets(cfg *spacemanager.DeployConfig, errOut writer) {
	keys := cfg.LiteralSecretKeys()
	if len(keys) == 0 {
		return
	}
	fmt.Fprintf(errOut, "sklp deploy: warning: %s written as literal value(s) in the deploy file; "+
		"they are stored in cleartext in the manifest. Use ${VAR} to keep them out of it.\n",
		strings.Join(keys, ", "))
}
