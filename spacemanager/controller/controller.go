// Package controller is the inter-space orchestration level: the SpaceController
// reconciles the set of spaces a host runs, owns the shared host gateway
// singleton, and exposes them through the control API. It is to []Space what
// spacemanager.Agent is to []Service — the same reconcile shape, one level up (see
// docs/sklp/space-controller-spec.md).
//
// The controller is host-level: it owns the shared gateway singleton (via a
// ref-counted gatewayManager) and runs N spaces in parallel, each on its own
// goroutine. Every per-space resource (runc launcher, Space, control socket,
// lock, manifest) is already scoped by space name, so the spaces are isolated
// by construction; the ONLY cross-space coordination is the gateway lifecycle,
// which the ref count handles (up on first space, down on last).
package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/lalternativefabrique/sklp/internal/xdg"
	"github.com/lalternativefabrique/sklp/space"
	"github.com/lalternativefabrique/sklp/spacemanager"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
	"strings"
)

// proxyDynDir is the per-space traefik file-provider dir. Kept identical to the
// previous inline path so the extraction is byte-for-byte parity.
func proxyDynDir(space string) string {
	return filepath.Join(os.TempDir(), "sklp-deploy", space, "traefik")
}

// SpaceOptions carries everything a SpaceController needs to materialise one
// space, mirroring the inline supervise path's inputs. Exported so the caller
// (cmd/sklp) composes it without reaching into deploy internals.
type SpaceOptions struct {
	// Meta is the space identity (name + routing metadata).
	Meta spacemanager.SpaceMeta
	// Services is the desired service set for this space (the stack).
	Services []spacemanager.Service

	// MeshResolver resolves a mesh gateway-route target ("<space>/<service>")
	// to its hosting node's mesh IP; forwarded to the agent. nil = mesh routes
	// stay unrouted.
	MeshResolver func(space, service string) (string, bool)
	// GatewayDriver, when non-empty, is the host gateway driver to bring up
	// (e.g. "traefik"). The controller owns the gateway singleton: one EnsureUp,
	// torn down on teardown. Empty = no host gateway (loopback ingress only).
	GatewayDriver string
	// Proxied enables the per-space traefik file-provider proxy (sopts.proxied).
	Proxied bool

	// OnAllReady fires once when every service first reaches Ready.
	OnAllReady func()

	// Listen, when non-empty, also serves the control API on this TCP+TLS addr
	// (remote pilot), in addition to the local unix socket.
	Listen string

	// Out / ErrOut are the live-feedback streams (reporter + warnings).
	Out    *os.File
	ErrOut *os.File

	// Shutdown is invoked by the control API to request a graceful teardown of
	// this space (e.g. `sklp deploy delete`): it cancels the space's context so the
	// agent loop returns and the deferred teardown (unroute, gateway release,
	// socket removal) runs. This does NOT by itself remove the persisted manifest
	// or per-space cache — see PurgeOnShutdown.
	Shutdown func()

	// ShutdownAndPurge tears the space down AND removes its persisted manifest
	// and cache, so it does not return at the next boot — the deletion the
	// plain Shutdown deliberately is not. Set by AddSpace; a caller does not
	// provide it.
	ShutdownAndPurge func()

	// ShutdownAndDelete is ShutdownAndPurge plus the space's named volumes:
	// the data goes too, irreversibly. Set by AddSpace; a caller does not
	// provide it. Keeping the volumes back is what left the space dir behind
	// whenever they lived under it, and the inventory lists spaces by
	// directory name — so the deleted space came back as a stopped ghost.
	ShutdownAndDelete func()

	// PurgeOnShutdown, when true, makes AddSpace remove this space's persisted
	// manifest (control.RemoveSpaceArtifacts) and cache dir
	// (space.PurgeSpaceCache) once Shutdown has been called and runSpace has
	// returned — the same cleanup the LOCAL `sklet apply --down`/Down path
	// already does (sklet/down.go), which a remote `sklp deploy
	// delete` (POST /v1/down) never reached: that route only ever cancelled the
	// context, leaving the manifest behind so a later `apply` with an
	// unchanged spec reported "unchanged" against a space whose runtime was
	// actually stopped. False (the RunAll/systemd single-space path, which has
	// its own teardown via sklet.Down) leaves this off — only the multi-space
	// `sklet agent` path (agentSpaceOptions) opts in.
	PurgeOnShutdown bool

	// OnStopped, when non-nil, fires after runSpace has returned (space fully
	// torn down), regardless of PurgeOnShutdown. A multi-space caller
	// (agentSpaceOptions) uses this to Unregister the space from the shared
	// control.Registry — without it, a torn-down space's Supervisor stays
	// reachable in the registry, so a subsequent POST /v1/apply for the same
	// name is dispatched to the STALE Supervisor instead of materializing a
	// fresh one: its in-memory DesiredServiceNames() still lists the old
	// service, so the same manifest compares equal and the apply reports
	// "unchanged" against a space that, on disk, no longer exists.
	OnStopped func(space string)

	// NetListener, when non-nil, starts the opt-in network control listener (or,
	// for a multi-space caller, registers this space into a shared one — see
	// RunAll). Injected so cmd/sklp keeps owning the cert/token/print plumbing
	// (startNetListener) without the controller depending on it. shutdown is
	// THIS space's own teardown hook — RunAll's resolved o.Shutdown, i.e.
	// either the caller's own (SuperviseLoop) or the per-space child context's
	// cancel func RunAll generates when the caller left Shutdown nil (a
	// multi-space caller like RunAgent) — so a registered space's POST
	// /v1/down always reaches the right teardown regardless of which case
	// this is.
	NetListener func(ctx context.Context, space string, sup control.Supervisor, shutdown func())

	// NetListenerWithPurge, when set, is called instead of NetListener, and
	// additionally receives the deletion variant of the teardown so the
	// registered handle can serve both a stop and a delete.
	NetListenerWithPurge func(ctx context.Context, space string, sup control.Supervisor, shutdown, purgeShutdown func())

	// NetListenerWithDelete, when set, wins over both listeners above and
	// additionally receives the teardown that drops the named volumes, so the
	// registered handle can serve a stop, a delete, and a delete-with-data.
	NetListenerWithDelete func(ctx context.Context, space string, sup control.Supervisor, shutdown, purgeShutdown, deleteShutdown func())
}

// SpaceController manages the spaces a host runs. It owns the shared gateway
// singleton; each space it materialises gets the same host gateway via the
// ref-counted manager, so N spaces share one :443/:80 front door. It also
// tracks every space it has launched (active), so a space can be added AFTER
// RunAll's initial batch has already started blocking — see AddSpace, used by
// a multi-space control-plane node (sklet agent) to materialise a
// newly-deployed space without restarting the process.
type SpaceController struct {
	gw *gatewayManager

	mu     sync.Mutex
	wg     sync.WaitGroup
	active map[string]struct{}
	// purging holds, per deleted space, a channel closed once its dir is purged.
	purging map[string]chan struct{}

	errMu    sync.Mutex
	firstErr error
}

// New builds a host SpaceController.
func New() *SpaceController {
	return &SpaceController{gw: newGatewayManager(), active: map[string]struct{}{}}
}

// Run materialises ONE space and blocks on its agent loop until ctx is
// cancelled (parity with the previous inline single-space deploy path). It is a
// thin wrapper over RunAll for the common one-space case.
func (c *SpaceController) Run(ctx context.Context, opts SpaceOptions) error {
	return c.RunAll(ctx, []SpaceOptions{opts})
}

// RunAll materialises every space in opts and runs them in parallel, each on
// its own goroutine AND its own cancellable child context, sharing the host
// gateway. It blocks until every space's agent loop returns. Cancelling ctx
// tears every space down together (the single-space Run path's behaviour,
// preserved: one ctx, one space, cancelling it always meant "stop"). A
// per-space child context additionally lets ONE space stop independently of
// its siblings — required for a multi-space node (RunAgent), where POST
// /v1/down against space A must never take space B down with it. The first
// non-cancel error from any space is returned; the others are logged. Any
// space added later via AddSpace is folded into the same wait group, so a
// caller blocking on RunAll's return also waits out spaces added after it.
func (c *SpaceController) RunAll(ctx context.Context, spaces []SpaceOptions) error {
	if len(spaces) == 0 {
		return nil
	}
	// GC orphan gateway routes BEFORE any space declares its own: a supervisor
	// SIGKILLed before its teardown Unroute ran leaves a stale route file behind.
	// Prune once per distinct driver, keyed by the route set every live space
	// (persisted manifests + the spaces this call launches) actually declares, so
	// a crashed host self-heals without cutting a sibling space's routes.
	c.pruneOrphanRoutes(spaces)

	for _, opts := range spaces {
		// Errors are not handled here: AddSpace's goroutine already records
		// them via recordErr (which feeds RunAll's returned firstErr below) and
		// logs them to ErrOut before this call returns, so re-handling the
		// returned error here would print it twice.
		_ = c.AddSpace(ctx, defaults(opts))
	}
	c.wg.Wait()
	return c.firstErr
}

// AddSpace materialises ONE space and runs it on its own goroutine with its
// own cancellable child context (same derivation RunAll always used), then
// returns as soon as the space has registered itself as ready to serve (via
// NetListener) or failed before reaching that point — NOT when the space's
// agent loop eventually exits. This makes it safe to call after RunAll's
// initial batch is already blocking in wg.Wait(): a multi-space control-plane
// node (sklet agent) uses this to bring up a space discovered at request time
// (an unknown-space POST /v1/apply), instead of requiring a process restart
// to re-run RunAll with the new space included.
//
// Returns an error immediately if this space name is already active (a
// concurrent or duplicate AddSpace for the same space), so callers get
// idempotent, race-free "already starting" behaviour without their own
// locking.
func (c *SpaceController) AddSpace(ctx context.Context, opts SpaceOptions) error {
	name := opts.Meta.Name
	c.mu.Lock()
	for {
		purging := c.purging[name]
		if purging == nil {
			break
		}
		c.mu.Unlock()
		select {
		case <-purging:
		case <-ctx.Done():
			return ctx.Err()
		}
		c.mu.Lock()
	}
	if _, dup := c.active[name]; dup {
		c.mu.Unlock()
		return fmt.Errorf("space %q is already starting or running on this node", name)
	}
	c.active[name] = struct{}{}
	c.mu.Unlock()

	opts = defaults(opts)
	spaceCtx, cancel := context.WithCancel(ctx)
	// requested latches true the instant Shutdown is actually called (a
	// deliberate stop), as opposed to spaceCtx being cancelled some other way
	// (parent ctx done, host shutting down) — only the former should purge the
	// space's persisted state.
	var requested atomic.Bool
	// purge latches when the teardown was asked to DELETE the space rather
	// than stop it. Stopping keeps the manifest so the space can be started
	// again; deleting removes it so it does not come back at the next boot.
	// Both arrive on the same route, so the intent has to travel with the
	// request — see control.DownRequest.
	var purge atomic.Bool
	// volumes latches when the deletion was asked to take the named volumes
	// with it. Only read alongside purge: a stop must never drop data.
	var volumes atomic.Bool
	callerShutdown := opts.Shutdown
	opts.Shutdown = func() {
		requested.Store(true)
		if callerShutdown != nil {
			callerShutdown()
		}
		cancel()
	}
	// ShutdownAndPurge is the deletion variant the control API calls for
	// `sklp deploy delete`; plain Shutdown stops the space and keeps it.
	opts.ShutdownAndPurge = func() {
		purge.Store(true)
		opts.Shutdown()
	}
	// ShutdownAndDelete is the variant that also drops the named volumes —
	// what the UI's Delete button and `sklp deploy delete -v` mean.
	opts.ShutdownAndDelete = func() {
		volumes.Store(true)
		opts.ShutdownAndPurge()
	}

	// ready is filled exactly once, by whichever of these happens first inside
	// runSpace (they never race: runSpace calls NetListener strictly AFTER its
	// early setup checks, e.g. requireDurableVolumes — see runSpace below):
	//   - NetListener firing: the space reached the point where it registers
	//     itself as servable — this is the space's REAL readiness signal, and
	//     AddSpace always installs one (wrapping the caller's own, if any) so
	//     this is the common case.
	//   - runSpace returning an error before ever reaching NetListener: an
	//     early setup failure (e.g. requireDurableVolumes) — must unblock
	//     AddSpace with that error, not leave it hanging.
	ready := make(chan error, 1)
	var readyOnce sync.Once
	signalReady := func(err error) { readyOnce.Do(func() { ready <- err }) }
	opts = wrapListeners(opts, signalReady)

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer cancel()
		err := c.runSpace(spaceCtx, opts)
		// Free the name BEFORE unblocking a waiting AddSpace caller: a caller
		// that gets an error back and immediately retries (e.g. a corrected
		// redeploy) must never see "already starting" for a space that, from
		// its perspective, has already failed and gone away.
		// A deleted space's dir is purged after its name is freed; an apply of
		// the same name waits for that purge, or it would start a worker whose
		// attach socket the purge then removes from under it.
		willPurge := opts.PurgeOnShutdown && requested.Load() && purge.Load()
		purged := make(chan struct{})
		c.mu.Lock()
		delete(c.active, name)
		if willPurge {
			if c.purging == nil {
				c.purging = map[string]chan struct{}{}
			}
			c.purging[name] = purged
		}
		c.mu.Unlock()
		defer func() {
			if willPurge {
				c.mu.Lock()
				delete(c.purging, name)
				c.mu.Unlock()
				close(purged)
			}
		}()
		// Unconditionally, regardless of PurgeOnShutdown or why runSpace
		// returned (deliberate stop, crash, or the parent ctx dying some
		// other way): a stopped space must never stay reachable through
		// whatever registered it (e.g. control.Registry for a multi-space
		// network dispatcher) — see OnStopped's doc for the "unchanged"
		// bug a stale registration causes.
		if opts.OnStopped != nil {
			opts.OnStopped(name)
		}
		// A deliberate Shutdown (not a crash, not the parent ctx dying some
		// other way) purges this space's persisted state — AFTER runSpace has
		// returned, so containers are already torn down and this never races
		// a still-running instance. Best-effort: a leftover only wastes disk,
		// same tolerance sklet.Down already has for its own cleanup steps.
		if willPurge {
			if perr := control.RemoveSpaceArtifacts(name); perr != nil {
				fmt.Fprintf(opts.ErrOut, "sklp deploy: warning: could not remove artifacts for space %q: %v\n", name, perr)
			}
			// Keep the volumes unless the teardown explicitly asked for them:
			// kept, they hold the space dir alive and the inventory (which
			// lists dir names) shows the deleted space as a stopped ghost.
			if perr := space.PurgeSpaceCache(name, !volumes.Load()); perr != nil {
				fmt.Fprintf(opts.ErrOut, "sklp deploy: warning: could not purge cache for space %q: %v\n", name, perr)
			}
		}
		// runSpace returning before NetListener ever fired (e.g. it failed
		// during setup) still must unblock a caller waiting on AddSpace. A
		// NetListener that already fired makes this a no-op (readyOnce).
		signalReady(err)
		if err != nil {
			c.recordErr(name, err, opts.ErrOut)
		}
	}()

	return <-ready
}

// recordErr logs a space's failure and keeps the first one for RunAll's
// return value, mirroring RunAll's previous inline error aggregation.
// wrapListeners makes whichever listener runSpace will call signal readiness:
// runSpace prefers NetListenerWithPurge, so wrapping NetListener alone left
// AddSpace waiting for a signal that only came when the space stopped — and
// RunAll, which adds spaces one after another, never got past the first.
func wrapListeners(opts SpaceOptions, signalReady func(error)) SpaceOptions {
	if callerDelete := opts.NetListenerWithDelete; callerDelete != nil {
		opts.NetListenerWithDelete = func(lctx context.Context, space string, sup control.Supervisor, shutdown, purgeShutdown, deleteShutdown func()) {
			callerDelete(lctx, space, sup, shutdown, purgeShutdown, deleteShutdown)
			signalReady(nil)
		}
		return opts
	}
	if callerPurge := opts.NetListenerWithPurge; callerPurge != nil {
		opts.NetListenerWithPurge = func(lctx context.Context, space string, sup control.Supervisor, shutdown, purgeShutdown func()) {
			callerPurge(lctx, space, sup, shutdown, purgeShutdown)
			signalReady(nil)
		}
		return opts
	}
	callerListener := opts.NetListener
	opts.NetListener = func(lctx context.Context, space string, sup control.Supervisor, shutdown func()) {
		if callerListener != nil {
			callerListener(lctx, space, sup, shutdown)
		}
		signalReady(nil)
	}
	return opts
}

func (c *SpaceController) recordErr(space string, err error, errOut *os.File) {
	fmt.Fprintf(errOut, "sklp deploy: space %q: %v\n", space, err)
	c.errMu.Lock()
	if c.firstErr == nil {
		c.firstErr = err
	}
	c.errMu.Unlock()
}

// pruneOrphanRoutes sweeps stale gateway route files left by a supervisor that
// died (SIGKILL) before its teardown Unroute ran. The keep-set is the union of
// the routes every space this call launches declares AND every OTHER live space
// already running on the host (read from its persisted manifest) — so the sweep
// reconciles the on-disk route set to the real desired set across processes,
// never deleting a sibling space's still-wanted route. Best-effort: any error is
// logged and the deploy proceeds (a leftover route only misroutes until the next
// re-apply rewrites it).
func (c *SpaceController) pruneOrphanRoutes(spaces []SpaceOptions) {
	// Group the keep-set by gateway driver: a host runs one driver in practice,
	// but two distinct drivers each own their own dyn dir, so prune per driver.
	keepByDriver := map[string]map[spacemanager.RouteKey]bool{}
	add := func(driver, space string, routes []spacemanager.ResolvedRoute) {
		if driver == "" {
			return
		}
		keep := keepByDriver[driver]
		if keep == nil {
			keep = map[spacemanager.RouteKey]bool{}
			keepByDriver[driver] = keep
		}
		for _, svc := range routedServices(routes) {
			keep[spacemanager.RouteKey{Space: space, Service: svc}] = true
		}
	}

	// This call's spaces (their sockets are not listening yet, so they would not
	// appear in the live-space scan below — add them explicitly). A per-space-mode
	// space has NO host singleton route file to GC (its ingress is injected), so
	// skip it — otherwise acquire() below would EnsureUp the very singleton this
	// mode exists to avoid, fighting the mux for :9080.
	launching := map[string]bool{}
	for _, o := range spaces {
		launching[o.Meta.Name] = true
		if o.Meta.GatewayMode == spacemanager.GatewayModePerSpace {
			continue
		}
		add(o.GatewayDriver, o.Meta.Name, o.Meta.Routes)
	}

	// Every OTHER live space already running on the host: keep its routes too.
	live, err := control.ListSpaces()
	if err == nil {
		for _, name := range live {
			if launching[name] {
				continue
			}
			_, meta, lerr := control.LoadManifest(name)
			if lerr != nil {
				continue
			}
			if meta.GatewayMode == spacemanager.GatewayModePerSpace {
				continue // per-space mode: no singleton route file to prune
			}
			driver, gerr := control.LoadGatewayDriver(name)
			if gerr != nil || driver == "" {
				continue
			}
			add(driver, name, meta.Routes)
		}
	}

	for driver, keep := range keepByDriver {
		gw, gerr := c.gw.acquire(driver)
		if gerr != nil {
			continue // gateway not up: nothing materialised to prune
		}
		if perr := gw.PruneRoutes(keep); perr != nil {
			fmt.Fprintf(os.Stderr, "sklp deploy: gateway route GC (%s): %v\n", driver, perr)
		}
	}
}

// defaults fills the feedback streams when the caller omitted them.
func defaults(o SpaceOptions) SpaceOptions {
	if o.Out == nil {
		o.Out = os.Stdout
	}
	if o.ErrOut == nil {
		o.ErrOut = os.Stderr
	}
	return o
}

// runSpace materialises one space and blocks on its agent loop until ctx is
// cancelled, then tears down (gateway ref release, control socket). It is the
// Lot 1 inline body, with the gateway now acquired/released through the shared
// ref-counted manager instead of a per-space EnsureUp/Down.
func (c *SpaceController) runSpace(ctx context.Context, o SpaceOptions) error {
	meta := o.Meta

	if err := requireDurableVolumes(o.Services); err != nil {
		return err
	}
	if err := requireTrustedImages(o.Services); err != nil {
		return err
	}

	launcher, ready, err := spacemanager.NewRuncLauncherWithEgress(meta.Name, meta.AllowEgress)
	if err != nil {
		return fmt.Errorf("init runc launcher: %w", err)
	}

	// Inject private-registry pull credentials (deploy.yaml `registries:`),
	// resolving each password from THIS process's env at launch (secret-safe: the
	// manifest carried only the env var name). The launcher attaches them to every
	// image pull. No-op when the space declares no registries.
	if ra, ok := launcher.(interface {
		SetRegistryAuths(map[string]space.RegistryAuth)
	}); ok {
		ra.SetRegistryAuths(spacemanager.ResolveRegistryAuthsWith(meta.Registries, control.SpaceSecretLookup(meta.Name)))
	}

	// Live feedback via the shared runner/reporter.
	rep := spacemanager.NewReporterObserver(o.Out, meta, o.Services)
	otelObs, otelShutdown, oerr := spacemanager.NewOtelObserver(context.Background(), meta)
	if oerr != nil {
		fmt.Fprintln(o.ErrOut, "sklp deploy: telemetry disabled:", oerr)
		otelObs, otelShutdown = nil, func() {}
	}
	defer otelShutdown()

	var agent *spacemanager.Agent
	opts := spacemanager.Options{
		OnRollout: func(rec spacemanager.RolloutRecord) {
			if rec.Result != spacemanager.RolloutRolledBack || agent == nil {
				return
			}
			if err := control.RepersistLive(meta.Name, agent.DesiredServices()); err != nil {
				fmt.Fprintf(o.ErrOut, "sklp deploy: persist manifest after rollback of %s: %v\n", rec.Service, err)
			}
		},
		Observer:     spacemanager.ComposeObservers(rep.Observer(), otelObs),
		LogSink:      rep.LogSink(),
		OnAllReady:   o.OnAllReady,
		Space:        meta.Name, // carried into every Domain the gateway routes
		Routes:       meta.Routes,
		MeshResolver: o.MeshResolver,
		GatewayMode:  meta.GatewayMode,
	}

	if o.Proxied {
		dynDir := proxyDynDir(meta.Name)
		proxy, perr := spacemanager.NewTraefikProxy(dynDir, o.Services)
		if perr != nil {
			return fmt.Errorf("init proxy: %w", perr)
		}
		opts.Proxy = proxy
	}

	// Host-gateway front door: the controller owns the singleton, shared across
	// spaces via the ref-counted manager. EnsureUp on the first space that needs
	// it, Down on the last — so one space's teardown never cuts the gateway out
	// from under the others. The agent only declares routes through the Gateway.
	//
	// SKIP the singleton in per-space mode: the front door is then the host mux
	// (`sklp gateway mux`) and the L7 ingress is injected INTO the space, so
	// bringing up a :9080 singleton would fight the mux for the port. The mode
	// travels in the persisted manifest, so the detached supervisor honours it.
	if o.GatewayDriver != "" && o.Meta.GatewayMode != spacemanager.GatewayModePerSpace {
		gw, gerr := c.gw.acquire(o.GatewayDriver)
		if gerr != nil {
			return fmt.Errorf("open gateway: %w", gerr)
		}
		defer func() {
			// Drop THIS space's routes on teardown so no stale router points at a
			// dead backend. Per (space, service), so sibling spaces on the shared
			// gateway are untouched. The gateway itself is a host singleton and is
			// NOT stopped here — only an explicit `sklp gateway down` tears it down,
			// so deleting one space never cuts the front door from the others.
			// Best-effort: a leftover route file only misroutes until re-apply.
			for _, svc := range routedServices(meta.Routes) {
				if uerr := gw.Unroute(meta.Name, svc); uerr != nil {
					fmt.Fprintf(o.ErrOut, "sklp deploy: gateway unroute %s/%s: %v\n", meta.Name, svc, uerr)
				}
			}
		}()
		opts.Gateway = gw
	}

	agent = spacemanager.NewAgent(launcher, ready, o.Services, opts)

	// Local control socket so `sklp deploy ps` / `logs` / re-apply talk to THIS
	// supervisor. Best-effort: a socket failure leaves the deploy running.
	if ln, socket, lerr := control.Listen(meta.Name); lerr != nil {
		fmt.Fprintf(o.ErrOut, "sklp deploy: control API disabled (ps/logs unavailable): %v\n", lerr)
	} else {
		defer os.Remove(socket)
		go func() {
			if serr := control.ServeWithPurge(ctx, meta.Name, agent, ln, o.Shutdown, o.ShutdownAndPurge); serr != nil {
				fmt.Fprintln(o.ErrOut, "sklp deploy: control API stopped:", serr)
			}
		}()
	}

	// Opt-in network control listener (remote pilot), in addition to the socket.
	// NetListener alone gates this, not Listen: a multi-space caller (RunAgent)
	// opens ONE shared listener across every space and uses NetListener purely
	// to register each space's freshly-built agent into that shared registry,
	// with Listen left "" so THIS function never opens a competing per-space
	// port. A single-space caller (SuperviseLoop) still sets both together.
	if o.NetListener != nil || o.NetListenerWithPurge != nil || o.NetListenerWithDelete != nil {
		if o.NetListenerWithDelete != nil {
			o.NetListenerWithDelete(ctx, meta.Name, agent, o.Shutdown, o.ShutdownAndPurge, o.ShutdownAndDelete)
		} else if o.NetListenerWithPurge != nil {
			o.NetListenerWithPurge(ctx, meta.Name, agent, o.Shutdown, o.ShutdownAndPurge)
		} else {
			o.NetListener(ctx, meta.Name, agent, o.Shutdown)
		}
	}

	// Say which volumes nothing backs up. A space can be deployed with a database
	// and no backup at all, and the only moment anyone finds out is the one where
	// it is already too late — so the node reports it every time it brings the
	// space up, rather than staying silent about data it cannot restore.
	if unbacked := spacemanager.UnbackedVolumes(o.Services); len(unbacked) > 0 {
		fmt.Fprintf(o.ErrOut, "sklp deploy: space %q: no backup declared for %s — this data cannot be restored if the node is lost\n",
			meta.Name, strings.Join(unbacked, ", "))
	}

	go spacemanager.StartBackupScheduler(ctx, agent, func(format string, a ...any) {
		fmt.Fprintf(o.Out, format, a...)
	})

	runErr := agent.Run(ctx)
	rep.Finalize(meta, o.Services, agent.RecentLogLines)
	if runErr != nil && runErr != context.Canceled {
		return runErr
	}
	return nil
}

// routedServices returns the distinct service names targeted by a space's
// gateway routes, in deterministic order, so teardown unroutes each exactly
// once.
func routedServices(routes []spacemanager.ResolvedRoute) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, r := range routes {
		if _, ok := seen[r.Service]; ok {
			continue
		}
		seen[r.Service] = struct{}{}
		out = append(out, r.Service)
	}
	sort.Strings(out)
	return out
}

// requireDurableVolumes refuses to bring up a space whose services declare named
// volumes when the node has no data disk configured.
//
// Without one the volumes land under the runtime cache, which `sklp clean` is
// built to empty and which a provisioned node keeps on the root disk rather than
// the separate volume bought so data survives the VM. Failing the apply says so
// while an operator is watching; the alternative is a database that looks fine
// until the day someone reclaims disk space.
func requireDurableVolumes(services []spacemanager.Service) error {
	if xdg.DataRoot() != "" {
		return nil
	}
	var named []string
	for _, svc := range services {
		for _, v := range svc.Volumes {
			if isNamedVolume(v) {
				named = append(named, svc.Name+":"+v)
			}
		}
	}
	if len(named) == 0 {
		return nil
	}
	return spacemanager.ConfigError(fmt.Errorf(
		"this node stores no data durably, but %s declares named volume(s) %s.\n"+
			"Run `sudo sklet host setup` to configure the data disk, or set %s to a path outside the runtime cache",
		pluralServices(named), strings.Join(named, ", "), xdg.DataRootEnv))
}

// requireTrustedImages applies the node's image policy where every bring-up
// converges: a fresh supervisor, an agent restart replaying persisted
// manifests. The apply edge checks the same policy for a live supervisor.
func requireTrustedImages(services []spacemanager.Service) error {
	policy := spacemanager.ImagePolicyFromEnv()
	for _, svc := range services {
		if err := policy.Check(svc.Image); err != nil {
			return spacemanager.ConfigError(fmt.Errorf("service %q: %w", svc.Name, err))
		}
	}
	return nil
}

// isNamedVolume reports whether a `volumes:` entry is a NAMED volume rather than
// a host bind. A bind ("/host/path:/ctr") already names a durable location the
// operator chose; only a named volume is materialised by sklp, so only it can
// land in the cache.
func isNamedVolume(entry string) bool {
	return !strings.HasPrefix(entry, "/") && !strings.HasPrefix(entry, "./") && !strings.HasPrefix(entry, "~")
}

func pluralServices(named []string) string {
	if len(named) == 1 {
		return "a service"
	}
	return "services"
}
