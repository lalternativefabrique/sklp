package devstack

// Embedded-runtime launch path for `sklp dev`.
//
// When the binary is built with -tags embedded_runtime on Linux AND the host
// can run rootless runc (ChooseDevBackend succeeds), `sklp dev` runs every
// service in the SAME runc runtime + spacenet that `sklp deploy` and the CI
// runner use — instead of docker (image:) + host processes (run:). This gives
// dev↔CI↔deploy parity: rootless runc, spacenet DNS-by-name between services, the
// skt toolchain for run: services, and the repo bind-mounted for hot-reload.
//
// dev keeps its OWN policy, distinct from deploy: it launches a service once,
// gates dependents on readiness, tails logs, SURFACES a crash (red) — and does
// NOT reconcile/restart/replicate (that is deploy). dev = an iteration loop.
//
// This file drives only the backend-neutral devBackend interface, so it is
// untagged and unit-testable with a fake backend (no real container). The
// legacy docker/process-host path in main.go is the fallback selected when
// ChooseDevBackend declines.

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"
)

// embeddedRun is the embedded counterpart of Main's launch loop. It returns an
// exit code with the same semantics as Main (0 = clean stop / signal, 1 =
// launch or readiness failure). The caller has already resolved the stack, env
// files, printer, and signal channel; here we only drive the backend.
func embeddedRun(
	ctx context.Context, cancel context.CancelFunc,
	backend devBackend, space string,
	stack *Stack, repoAbs, stackName string,
	envFromFiles, baseEnv map[string]string,
	printer *logPrinter, sigs <-chan os.Signal, graceMs int,
) int {
	es := &embeddedState{backend: backend}
	backend.SetStopGrace(time.Duration(graceMs) * time.Millisecond)

	// scope is the per-worktree id every dev hostname folds in:
	// <svc>.<scope>.localhost. Two worktrees of the same repo get distinct scopes
	// (their abs paths differ) so their `*.localhost` URLs never collide.
	scope := devScopeID(repoAbs, stackName)

	shutdown := func(reason string) {
		printer.Banner("%s — shutting down", reason)
		cancel() // stops log tails + crash watchers
		es.stopAll(printer)
		backend.Close()
		removeState(repoAbs, stackName)
	}

	// Resolve the project toolchain once (skt installs go/node/pnpm on the host
	// from .sklp/config.yaml). A run: service then runs on a minimal base with
	// this toolchain bind-mounted — no per-project image to build. A failure
	// here is fatal: a declared toolchain that can't be provisioned must stop
	// the run, not silently launch a service that can't compile.
	toolchain, err := resolveDevToolchain(ctx, repoAbs, func(spec string) {
		printer.Banner("toolchain: resolving %s", spec)
	})
	if err != nil {
		printer.Banner("toolchain: %v", err)
		return 1
	}

	for _, ns := range stack.Services {
		if ns.Def.Optional {
			printer.Banner("[%s] optional — skipped", ns.Name)
			continue
		}

		// Container env: declared sources only (env files / pipeline / service),
		// NEVER the host shell's env. Leaking the host PATH would override the
		// image PATH (OCI last-wins) — e.g. postgres would no longer find initdb.
		env := composeContainerEnv(envFromFiles, baseEnv, ns.Def.Env)
		spec := devServiceSpec(ns, repoAbs, toolchain, env)

		printer.Banner("[%s] launching (embedded runc)", ns.Name)
		_, err := backend.StartService(ctx, spec)
		if err != nil {
			printer.Banner("[%s] launch failed: %v", ns.Name, err)
			shutdown(fmt.Sprintf("[%s] launch failed", ns.Name))
			return 1
		}
		es.add(ns.Name, ns.Def.Oneshot)
		// Tail the container log into a per-service broadcaster so the printer
		// (and a Log readiness probe) see the live output, prefixed by service.
		bcast := es.tail(ctx, ns.Name, printer)

		if err := writeState(repoAbs, stackName, es.snapshot(os.Getpid(), space)); err != nil {
			printer.Banner("warning: could not write dev run-state: %v", err)
		}

		// Oneshot: the container's exit IS the ready signal (a migration step).
		// We watch it to completion before unblocking dependents.
		if ns.Def.Oneshot {
			select {
			case sig := <-sigs:
				shutdown(fmt.Sprintf("received %s during [%s] oneshot", sig, ns.Name))
				return 0
			case <-backend.Watch(ctx, ns.Name):
				printer.Banner("[%s] completed", ns.Name)
			}
			continue
		}

		// Readiness gating: a dependent only launches after this service is
		// Ready (or, with no probe, as soon as its container init is up). We
		// race the probe against Ctrl-C and the service crashing, so neither a
		// 30s timeout nor a crash strands the user.
		if ns.Def.Ready != nil {
			// For a Log probe the embedded backend reads the captured log file;
			// keep the broadcaster alive so the printer still streams output.
			_ = bcast
			// The probe addresses the container directly (localhost:<port> ->
			// container IP, port kept by the runtime), so no port templating is
			// needed: the declared port IS the container port.
			probeDone := make(chan error, 1)
			probeCtx, cancelProbe := context.WithCancel(ctx)
			go func() { probeDone <- backend.WaitReady(probeCtx, ns.Name, ns.Def.Ready, ns.ReadyTimeout) }()
			watch := backend.Watch(probeCtx, ns.Name)
			start := time.Now()
			select {
			case sig := <-sigs:
				cancelProbe()
				shutdown(fmt.Sprintf("received %s during [%s] ready probe", sig, ns.Name))
				return 0
			case <-watch:
				cancelProbe()
				printer.Banner("[%s] exited before ready", ns.Name)
				shutdown(fmt.Sprintf("[%s] crashed during startup", ns.Name))
				return 1
			case err := <-probeDone:
				cancelProbe()
				if err != nil {
					printer.Banner("[%s] ready probe failed: %v", ns.Name, err)
					shutdown(fmt.Sprintf("[%s] not ready", ns.Name))
					return 1
				}
				printer.Banner("[%s] ready (%.1fs)", ns.Name, time.Since(start).Seconds())
			}
		}

		// Publish the service under <svc>.<scope>.localhost now that it is Ready,
		// so http://core.<scope>.localhost etc. reach it through the host mux.
		backend.RegisterHost(scope, ns.Name, spec.ContainerPort)
	}

	printer.Banner("✓ stack ready (embedded runc) — Ctrl-C to stop")

	// Steady state: surface the FIRST service crash (dev does not reconcile),
	// or tear down on Ctrl-C.
	crashed := es.watchAll(ctx)
	select {
	case sig := <-sigs:
		shutdown(fmt.Sprintf("received %s", sig))
	case name := <-crashed:
		shutdown(fmt.Sprintf("[%s] exited", name))
	}
	return 0
}

// embeddedState tracks the services launched in this run so the supervisor can
// tear them down (reverse order) and watch them for crashes. It is the embedded
// analogue of the legacy `supervisor`, minus the process-group machinery (the
// runc launcher owns the containers).
type embeddedState struct {
	backend devBackend
	names   []string
	oneshot map[string]bool // names that are run-once (jobs): a finished one is NOT a crash
	bcasts  map[string]*logBroadcaster
}

func (s *embeddedState) add(name string, oneshot bool) {
	s.names = append(s.names, name)
	if s.bcasts == nil {
		s.bcasts = map[string]*logBroadcaster{}
	}
	if s.oneshot == nil {
		s.oneshot = map[string]bool{}
	}
	s.oneshot[name] = oneshot
}

// tail starts streaming a service's container log into a fresh broadcaster and
// onward to the printer (one subscriber). Returns the broadcaster so a Log
// readiness probe could subscribe too. The tail goroutine stops when ctx is
// cancelled (teardown).
func (s *embeddedState) tail(ctx context.Context, name string, printer *logPrinter) *logBroadcaster {
	b := newLogBroadcaster()
	s.bcasts[name] = b
	sub := b.Subscribe()
	go func() {
		for line := range sub {
			printer.Print(name, line)
		}
	}()
	go tailLog(ctx, s.backend.LogPath(name), b)
	return b
}

// snapshot builds the runState an embedded run leaves for `sklp dev down`. The
// space name lets a dead-supervisor down reap orphaned containers; no pgids /
// OCI names are recorded (the runc launcher owns the containers).
func (s *embeddedState) snapshot(supervisorPID int, space string) runState {
	return runState{SupervisorPID: supervisorPID, Embedded: true, EmbeddedSpace: space}
}

// stopAll stops every launched service at once and reports each one as it
// goes, so a service holding out for its grace period never hides the others.
func (s *embeddedState) stopAll(printer *logPrinter) {
	var wg sync.WaitGroup
	for _, name := range s.names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			start := time.Now()
			s.backend.StopService(name)
			printer.Banner("[%s] stopped (%.1fs)", name, time.Since(start).Seconds())
		}(name)
	}
	wg.Wait()
}

// watchAll returns a channel that delivers the name of the FIRST long-running
// service to exit. dev surfaces it (red) and tears down — it never restarts.
// Oneshot services (jobs) are EXCLUDED: a job exits 0 by design once its work
// is done (migrate, seed), and that completion must not be read as a crash that
// tears the stack down. Jobs are already gated to completion during startup.
func (s *embeddedState) watchAll(ctx context.Context) <-chan string {
	out := make(chan string, 1)
	for _, name := range s.names {
		if s.oneshot[name] {
			continue // a finished job is not a crash
		}
		name := name
		go func() {
			<-s.backend.Watch(ctx, name)
			select {
			case out <- name:
			default:
			}
		}()
	}
	return out
}
