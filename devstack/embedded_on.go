//go:build linux

package devstack

import (
	"context"
	"fmt"
	"os"
	"path"
	"sync"
	"time"

	"github.com/lalternativefabrique/sklp/internal/toolchain"
	"github.com/lalternativefabrique/sklp/pipeline"
	"github.com/lalternativefabrique/sklp/pipeline/runnerimage"
	"github.com/lalternativefabrique/sklp/space"
	"github.com/lalternativefabrique/sklp/space/runnerbuild"
)

// embeddedBackend drives `sklp dev` services through the SAME runc launcher +
// spacenet that sklp deploy and the CI runner use. This is what gives dev↔CI↔
// deploy parity: rootless runc, spacenet DNS-by-name between services, and the
// repo bind-mounted (opt-in) for hot-reload of run: services.
//
// It is a thin adapter over space.Space: the launcher already owns
// image pull, the OCI spec, the space bridge, per-name DNS, detached launch,
// log capture, readiness probing, host ingress, and teardown. embeddedBackend
// only maps the dev-neutral DevServiceSpec onto space.DeployService —
// crucially filling the dev-only DevMounts/WorkdirOverride fields that deploy
// leaves empty (so the deploy spec stays host-bind-free; see runtime/spec.go).
//
// Unlike deploy, dev has NO reconciliation/replicas/systemd: it launches a
// service once, gates dependents on readiness, surfaces a crash (red), and
// tears everything down on Ctrl-C. The launcher is the shared mechanism; the
// dev policy (no auto-restart) lives in the supervisor loop, not here.
type embeddedBackend struct {
	l   *space.Space
	mux *space.HostMux // shared :8080 host mux; nil if the bind failed

	mu    sync.Mutex
	ports map[string]int // service -> container port (from its HTTP ready probe)
	hosts []string       // hostnames registered on the mux, for teardown

	buildMu     sync.Mutex                    // serialises + memoises runner rootfs builds
	builtRootfs map[string]runnerbuild.Result // recipe hash -> built rootfs result
}

// chooseDevBackend builds a space-scoped launcher so dev services share one
// spacenet bridge and resolve each other by name (core reaches postgres at
// "postgres", etc. — the same DNS contract as deploy). A launcher build
// failure declines gracefully so `sklp dev` falls back to docker/process-host.
// It also acquires the shared host mux (the :8080 hostname ingress); a mux bind
// failure is non-fatal — services still run, just without `*.localhost` URLs.
func chooseDevBackend(spaceName string, allowHost bool) (devBackend, bool) {
	if spaceName == "" {
		spaceName = "sklp-dev"
	}
	l, err := space.NewSpace(spaceName)
	if err != nil {
		return nil, false
	}
	// `sklp dev` is the only caller that may open the host LAN: deploy and CI
	// never reach this function.
	if allowHost {
		l.AllowHostAccess()
		fmt.Fprintf(os.Stderr, "[sklp dev] allowHost: services may reach this machine's LAN addresses "+
			"(cloud metadata stays blocked)\n")
	}
	// Per-worktree mux port: derived from the space name (stable per worktree, so
	// the URL is bookmarkable) with a bind-time +1 fallback. Two `sklp dev`
	// worktrees are two processes with two ports — both stay host-reachable at
	// once, fixing the parallel-worktree collision a fixed :8080 would cause.
	mux, merr := space.AcquireHostMux(space.HostMuxPortFor(spaceName))
	if merr != nil {
		fmt.Fprintf(os.Stderr, "[sklp dev] host mux unavailable (%v); services reachable in-space by name only\n", merr)
		mux = nil
	}
	return &embeddedBackend{l: l, mux: mux, ports: map[string]int{}, builtRootfs: map[string]runnerbuild.Result{}}, true
}

// embeddedAvailable is true on the tagged Linux build. The actual host
// capability (rootless userns) is proven lazily by chooseDevBackend building
// the launcher; this reports only that the code path is compiled in.
func embeddedAvailable() bool { return true }

// StartService maps a DevServiceSpec onto a space.DeployService and launches
// it. For an image: service Image is set and DevMounts stays nil (no host bind
// — deploy parity). For a run: service the toolchain image runs `sh -c <cmd>`
// with the repo bind-mounted rw at RepoMount for hot-reload. The service's
// container port (from its HTTP probe) is recorded here so a dependent's env
// rewrite resolves it and RegisterHost can later splice the mux to it.
func (b *embeddedBackend) StartService(ctx context.Context, spec DevServiceSpec) (int, error) {
	// Record this service's container port so RegisterHost can later splice the
	// host mux to it. Services with no exposed port carry 0 (not host-exposed).
	if spec.ContainerPort > 0 {
		b.mu.Lock()
		b.ports[spec.Name] = spec.ContainerPort
		b.mu.Unlock()
	}

	// A service's env is used verbatim: a service reaches a sibling by its space
	// DNS name (`core:4101`, `postgres:5432`), the same contract sklp deploy and
	// the CI runner use. There is no localhost→DNS rewrite — a config addresses
	// siblings by name, never by localhost (which, inside the service's own netns,
	// resolves to the service itself, not its peer).

	// A run: service flagged for the built runner rootfs: build it (once per
	// recipe hash), then launch on it via RootfsDir. The baked toolchain +
	// packages replace the minimal-base + bind-mount path entirely.
	if spec.RunnerRecipe != nil {
		return b.startOnRunnerRootfs(ctx, spec)
	}

	ds := space.DeployService{Name: spec.Name, Env: spec.Env, Port: spec.ContainerPort}
	isRun := spec.ShellCmd != ""
	if !isRun {
		ds.Image = spec.Image
		ds.Command = spec.Command
	} else {
		// run: service — minimal glibc base + repo bind-mount + shell command.
		// The base image carries no app and no toolchain; both are mounted.
		// config.yaml pipeline.env (e.g. GOPRIVATE) applies so go treats the
		// project's private modules correctly — same source as CI.
		ds.Image = spec.Image
		ds.Env = applyConfigEnv(ds.Env, spec.ConfigEnv)
		// A private-module fetch needs git auth. If config.yaml declared a
		// GITHUB_TOKEN secret whose value is in this service's env, prepend a
		// preamble that writes ~/.netrc + forces an HTTPS rewrite, so git/go
		// can clone private repos. Mirrors CI's build-time netrc.
		ds.Command = []string{"sh", "-c", privateAuthPreamble(spec.Secrets, ds.Env) + spec.ShellCmd}
	}
	if spec.RepoMount != "" && spec.HostRepo != "" {
		// OPT-IN host bind, dev only: the container writes to the repo as the
		// mapped host uid (uid 0 -> operator), never root. cwd = the mount +
		// the service's workdir so `go run .` / `pnpm dev` run in the right
		// subdir (e.g. services/api), not the repo root.
		ds.DevMounts = []space.DevMount{{Source: spec.HostRepo, Destination: spec.RepoMount}}
		ds.WorkdirOverride = containerWorkdir(spec)
	}
	if spec.SktCacheMount != "" {
		// The toolchain (go/node/pnpm) is NOT in the base image — bind-mount the
		// host skt cache at the SAME path so its binaries (and pnpm's absolute
		// shebangs) resolve, then prepend the resolved bin dirs to PATH so the
		// pinned tools shadow anything the base provides.
		ds.DevMounts = append(ds.DevMounts, space.DevMount{Source: spec.SktCacheMount, Destination: spec.SktCacheMount, ReadOnly: true})
		ds.Env = prependToolchainPATH(ds.Env, spec.ToolchainBinDirs)
	}
	if isRun {
		ds.DevMounts, ds.Env = b.applyRunEnv(ds.DevMounts, ds.Env)
	}
	// Declared volumes back the service's data paths now that the rootfs is
	// read-only. The launcher (space runtime) resolves them to rw
	// bind-mounts; here we only pass them through. See runtime/volumes.go.
	ds.Volumes = spec.Volumes

	pid, err := b.l.Start(ctx, ds)
	if err != nil {
		return 0, err
	}
	return pid, nil
}

// applyRunEnv wires the shared run:-service environment for the embedded space:
// a PERSISTENT build cache bind-mount (so go/pnpm don't re-download every run),
// the cache + HOME env pointing into it, non-interactive flags (no TTY in the
// space), and the host CA bundle (the rootfs may lack public roots). Returns the
// augmented mounts + env. Used by both the minimal-base and runner-rootfs paths.
func (b *embeddedBackend) applyRunEnv(mounts []space.DevMount, env []string) ([]space.DevMount, []string) {
	// Persistent build cache: a host dir bind-mounted at a fixed in-container
	// path. go/pnpm caches point here (survives container exit), instead of the
	// ephemeral /tmp tmpfs the read-only rootfs otherwise forces.
	host := devBuildCacheDir()
	if err := os.MkdirAll(host, 0o755); err == nil {
		mounts = append(mounts, space.DevMount{Source: host, Destination: devBuildCacheMount})
		env = withPersistentBuildCaches(env)
	}
	// HOME + remaining caches (HOME=/tmp etc.) for anything not redirected above;
	// hasEnvKey means the persistent values already set are kept.
	env = withWritableToolCaches(env)
	env = withNonInteractiveEnv(env)
	if caSrc, caDst, ok := hostCABundle(); ok {
		mounts = append(mounts, space.DevMount{Source: caSrc, Destination: caDst, ReadOnly: true})
		if !hasEnvKey(env, "SSL_CERT_FILE") {
			env = append(env, "SSL_CERT_FILE="+caDst)
		}
	}
	return mounts, env
}

// startOnRunnerRootfs builds (or reuses) the runner rootfs for spec.RunnerRecipe
// and launches the run: command on it. The rootfs bakes the toolchain + system
// packages, so unlike the minimal-base path there is NO skt cache bind-mount and
// no toolchain PATH prepend — the recipe's ENV (PATH, GOTOOLCHAIN, …) is applied
// as the image config. The repo bind-mount, private-auth netrc, writable tool
// caches, and CA bundle are kept (the rootfs is read-only and ships no app code
// nor, necessarily, CA roots beyond what the recipe installed).
func (b *embeddedBackend) startOnRunnerRootfs(ctx context.Context, spec DevServiceSpec) (int, error) {
	res, err := b.runnerRootfs(ctx, *spec.RunnerRecipe)
	if err != nil {
		return 0, fmt.Errorf("dev: build runner rootfs for %q: %w", spec.Name, err)
	}

	env := applyConfigEnv(spec.Env, spec.ConfigEnv)
	ds := space.DeployService{
		Name:      spec.Name,
		RootfsDir: res.Rootfs,
		RootfsCfg: space.ImageConfig{Env: bakedEnvSlice(res), WorkingDir: res.Workdir},
		Env:       env,
		Command:   []string{"sh", "-c", privateAuthPreamble(spec.Secrets, env) + spec.ShellCmd},
		Port:      spec.ContainerPort,
	}
	if spec.RepoMount != "" && spec.HostRepo != "" {
		ds.DevMounts = []space.DevMount{{Source: spec.HostRepo, Destination: spec.RepoMount}}
		ds.WorkdirOverride = containerWorkdir(spec)
	}
	ds.DevMounts, ds.Env = b.applyRunEnv(ds.DevMounts, ds.Env)
	ds.Volumes = spec.Volumes

	pid, err := b.l.Start(ctx, ds)
	if err != nil {
		return 0, err
	}
	return pid, nil
}

// runnerRootfs builds the runner rootfs for recipe r, memoised by recipe hash so
// several run: services sharing one recipe build it once. The base rootfs is the
// recipe's base_image (or the dev default when unset), extracted daemonless; the
// recipe's RUN steps run via the unshare step pipeline. Serialised under buildMu:
// a concurrent StartService for the same recipe waits rather than double-builds.
func (b *embeddedBackend) runnerRootfs(ctx context.Context, r runnerimage.Runner) (runnerbuild.Result, error) {
	// Force a concrete base_image so the recipe ALWAYS renders in from-base mode
	// (FROM <base> + RUN layers, no COPY). Without this an empty base_image routes
	// renderDockerfile to from-source — the legacy "recompile sklp from the
	// skalpai sources" path, which emits COPY and is meaningless for `sklp dev` on
	// a third-party project. The dev default base is used when the project ships
	// no runner base of its own.
	if r.BaseImage == "" {
		r.BaseImage = devBaseImage()
	}

	hash, err := runnerbuild.Hash(r)
	if err != nil {
		return runnerbuild.Result{}, err
	}

	b.buildMu.Lock()
	defer b.buildMu.Unlock()
	if res, ok := b.builtRootfs[hash]; ok {
		return res, nil
	}

	cache := b.l.CacheRoot()
	baseRootfs, err := space.PrepareImage(ctx, cache, r.BaseImage)
	if err != nil {
		return runnerbuild.Result{}, fmt.Errorf("prepare base %q: %w", r.BaseImage, err)
	}

	// The recipe's `RUN skt install …` steps need skt in the rootfs. A published
	// runner base bakes it; a plain dev base (debian-slim) does not, so bake the
	// host skt into the rootfs at the path the recipe expects. EnsureSkt prefers
	// the host PATH skt (dev), falling back to a baked one.
	var bake []runnerbuild.BakeFile
	if sktPath, serr := toolchain.EnsureSkt(); serr == nil {
		bake = append(bake, runnerbuild.BakeFile{Src: sktPath, Dest: runnerimage.SktBinPath(), Mode: 0o755})
	}

	res, err := runnerbuild.Build(ctx, cache, baseRootfs, r, bake, runnerbuild.UnshareStepRunner{})
	if err != nil {
		return runnerbuild.Result{}, err
	}
	b.builtRootfs[hash] = res
	return res, nil
}

// containerWorkdir is the in-container cwd for a run: service: the repo mount
// joined with the service's `workdir:` (repo-relative). No workdir → the mount
// root. Mirrors the host/CI paths which join repoRoot + svc.Workdir.
func containerWorkdir(spec DevServiceSpec) string {
	if spec.Workdir == "" {
		return spec.RepoMount
	}
	return path.Join(spec.RepoMount, spec.Workdir)
}

// bakedEnvSlice renders the recipe's ENV map into a KEY=value slice in the
// recipe's declared order (stable, matches the rendered Dockerfile order).
func bakedEnvSlice(res runnerbuild.Result) []string {
	out := make([]string, 0, len(res.Env))
	for _, k := range res.EnvOrder {
		out = append(out, k+"="+res.Env[k])
	}
	return out
}

// WaitReady gates dependents on this service's probe, using the SAME runc
// readiness path deploy uses (native dial from the space netns when possible,
// runc-exec fallback). The probe addresses the container directly (localhost is
// rewritten to the container IP by the runtime, keeping the declared port), so
// no host->container port translation is needed. A nil probe returns immediately.
func (b *embeddedBackend) WaitReady(ctx context.Context, name string, ready *pipeline.Ready, timeout time.Duration) error {
	return space.WaitServiceReady(ctx, name, readyToRuntime(ready, timeout), b.l.ProbeCoords(name))
}

// RegisterHost publishes the service once it is Ready:
//   - HTTP mux: <name>.<scope>.localhost:<muxPort> — HTTP-only, dispatched by
//     Host header. No-op when the mux is unavailable.
//   - TCP bind: 127.0.0.1:<containerPort> → container, for raw-TCP services
//     (postgres, nats, …). Reachable as <name>.<scope>.localhost:<port> or
//     simply localhost:<port>. A bind failure (port taken) is logged and skipped.
func (b *embeddedBackend) RegisterHost(scope, name string, containerPort int) {
	if containerPort == 0 {
		return
	}
	host := devHostname(scope, name)

	// HTTP mux — dispatch by hostname, HTTP only.
	if b.mux != nil {
		b.mux.RegisterHost(host, b.l, name, containerPort)
		b.mu.Lock()
		b.hosts = append(b.hosts, host)
		b.mu.Unlock()
		fmt.Printf("↥ %s → http://%s:%d\n", name, host, b.mux.BoundPort())
	}

	// TCP bind — one listener per service port, protocol-agnostic.
	b.l.OpenIngress(name, []space.PortMapping{{HostPort: containerPort, ContainerPort: containerPort}})
	fmt.Printf("↥ %s → %s:%d\n", name, host, containerPort)
}

// Watch polls the container's runc state and closes the returned channel when
// the service exits (or ctx is cancelled). dev surfaces the crash; it does NOT
// reconcile — the launcher is the shared mechanism, the no-restart policy is
// dev's. Polling (vs pidfd) keeps this simple and tag-local; the 500ms cadence
// is well within a dev iteration loop.
func (b *embeddedBackend) Watch(ctx context.Context, name string) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		// Running() collapses a transient `runc state` failure (state-dir
		// contention while another container of the space is being deleted —
		// the one-shot seed exiting, typically) into "not running". One such
		// read must not shut the whole stack down: require consecutive misses
		// before declaring the service dead. A real crash still surfaces in ~2s.
		misses := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if b.l.Running(name) {
					misses = 0
					continue
				}
				if misses++; misses >= 4 {
					return
				}
			}
		}
	}()
	return done
}

func (b *embeddedBackend) StopService(name string) { b.l.Stop(name) }

func (b *embeddedBackend) SetStopGrace(d time.Duration) { b.l.SetStopGrace(d) }

// Close unregisters every hostname this backend put on the shared mux, releases
// its mux reference (closing the :8080 listener only when the LAST worktree
// releases), and tears the space down. The order matters: stop routing host
// connections before the space netns (and its splice worker) vanishes.
func (b *embeddedBackend) Close() {
	if b.mux != nil {
		b.mu.Lock()
		hosts := b.hosts
		b.hosts = nil
		b.mu.Unlock()
		for _, h := range hosts {
			b.mux.UnregisterHost(h)
		}
		space.ReleaseHostMux()
	}
	b.l.Close()
}

func (b *embeddedBackend) LogPath(name string) string { return b.l.LogPath(name) }

// reapEmbeddedRun tears down an embedded dev run whose supervisor already died.
// Building a space launcher reaps every orphaned deploy-prefixed container on
// construction; Close then tears the space bridge down. Best-effort: a build
// failure means there is nothing reachable to reap, so down still reports clean.
func reapEmbeddedRun(spaceName string) {
	if spaceName == "" {
		spaceName = "sklp-dev"
	}
	l, err := space.NewSpace(spaceName)
	if err != nil {
		return
	}
	l.Close()
}

// readyToRuntime maps the runner-level Ready probe onto the runtime probe shape,
// applying the dev timeout when the probe didn't carry its own.
func readyToRuntime(r *pipeline.Ready, timeout time.Duration) *space.ServiceReady {
	if r == nil {
		return nil
	}
	out := &space.ServiceReady{HTTP: r.HTTP, TCP: r.TCP, Log: r.Log, File: r.File, Timeout: r.Timeout}
	if out.Timeout == "" && timeout > 0 {
		out.Timeout = timeout.String()
	}
	return out
}
