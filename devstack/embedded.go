package devstack

import (
	"context"
	"os"
	"time"

	"github.com/lalternativefabrique/sklp/pipeline"
	"github.com/lalternativefabrique/sklp/pipeline/runnerimage"
)

// devBackend is the lifecycle surface `sklp dev` needs from whatever runs a
// service. It is deliberately narrow and decoupled from runtime.Space
// so the untagged build (no embedded runtime) compiles and so the selection
// logic is testable with a fake backend — no real container is ever launched
// in a unit test.
//
// Two implementations exist:
//   - embeddedBackend (build tag embedded_runtime, Linux): drives the runc
//     launcher + spacenet, the SAME runtime sklp deploy and the CI runner use.
//     Gives dev↔CI↔deploy parity (rootless runc, spacenet DNS-by-name, skt
//     toolchain for run: services, repo bind-mount for hot-reload).
//   - the legacy path (proc.go startProc + oci.go buildOCIArgv): used as the
//     FALLBACK when the embedded runtime is not built in (default build, CI
//     image, non-Linux). Selected by ChooseDevBackend returning ok=false.
type devBackend interface {
	// StartService launches one service container and returns its init pid.
	// img: services run the image; run: services run a toolchain image (skt)
	// with the repo bind-mounted at spec.RepoMount. Long-lived (detached).
	StartService(ctx context.Context, spec DevServiceSpec) (pid int, err error)
	// WaitReady blocks until the service's readiness probe passes or the
	// timeout elapses (launch-and-forget when ready is nil). It probes the
	// container natively from the space netns — the same gate sklp deploy
	// uses — so a run: service waits for its deps exactly as today.
	WaitReady(ctx context.Context, name string, ready *pipeline.Ready, timeout time.Duration) error
	// RegisterHost publishes the service under <name>.<scope>.localhost on the
	// shared host mux once it is Ready: a host connection peeked to that name is
	// spliced into the space netns to the service's container port (derived from
	// its HTTP ready probe). No-op for a service with no HTTP probe (raw-TCP like
	// postgres stays reachable east-west by DNS only).
	RegisterHost(scope string, name string, containerPort int)
	// Watch returns a channel closed when the service container exits. dev uses
	// it to SURFACE a crash (red) — it never restarts the service (that is
	// deploy's job). The channel also closes when ctx is cancelled (teardown).
	Watch(ctx context.Context, name string) <-chan struct{}
	// StopService kills+removes one service container (idempotent).
	StopService(name string)
	// SetStopGrace bounds how long StopService waits after SIGTERM before SIGKILL.
	SetStopGrace(d time.Duration)
	// Close tears the shared space network + all containers + host
	// ingress down.
	Close()
	// LogPath is the host file capturing a service's stdout/stderr, so the
	// supervisor can tail it through the existing log broadcaster.
	LogPath(name string) string
}

// DevServiceSpec is the backend-neutral description of one dev service. It
// carries both the image: case (Image set, Command optional) and the run:
// case (ToolchainImage + ShellCmd + RepoMount for hot-reload). The selection
// and mapping logic lives in untagged code so it is unit-testable; only the
// actual runc calls live behind the build tag.
type DevServiceSpec struct {
	Name string
	// Image is the OCI ref for an image: service (postgres, nats, ...). For a
	// run: service it is the MINIMAL glibc base (debian-slim): the space
	// rootfs carries neither the app nor the toolchain — the app code arrives
	// by repo bind-mount and the toolchain by the skt cache bind-mount below.
	Image string
	// ShellCmd is the run: command, executed via `sh -c` in the space. Empty
	// for an image: service.
	ShellCmd string
	// ToolchainBinDirs are the host skt bin directories (go/node/pnpm resolved
	// from .sklp/config.yaml pipeline.toolchain) prepended to PATH for a run:
	// service. Empty for an image: service. The binaries are reached via the
	// SktCacheMount bind below — mounted at the SAME host path so absolute
	// shebangs (pnpm) resolve.
	ToolchainBinDirs []string
	// SktCacheMount is the host skt cache root, bind-mounted read-only at the
	// identical path inside a run: service so its toolchain binaries resolve.
	// Empty for an image: service.
	SktCacheMount string
	// ConfigEnv is config.yaml pipeline.env (e.g. GOPRIVATE), applied to a run:
	// service so go knows which modules are private. Empty for image: services.
	ConfigEnv map[string]string
	// Secrets is config.yaml pipeline.secrets (e.g. GITHUB_TOKEN). A run: service
	// turns a declared GITHUB_TOKEN (value from its env) into a ~/.netrc so git
	// can fetch private modules. Empty for image: services.
	Secrets []string
	// Command overrides the image entrypoint for an image: service.
	Command []string
	// Env is the KEY=value process environment.
	Env []string
	// RepoMount, when set, bind-mounts HostRepo at this in-container path for
	// hot-reload (run: services only). Empty = no host bind (image: services,
	// preserving the deploy security frontier).
	RepoMount string
	// HostRepo is the absolute host path bound at RepoMount. Set with RepoMount.
	HostRepo string
	// Workdir is the service's `workdir:` (repo-relative). Joined onto RepoMount
	// so `go run .` / `pnpm dev` run in the right subdir, not the repo root.
	Workdir string
	// ContainerPort is the port the service listens on inside its container,
	// derived from its HTTP ready probe (0 = no HTTP probe). The hostname mux
	// splices to it; run: env rewriting maps localhost:<port> to the space DNS
	// name on it. Hostname/ingress are derived from this, never declared.
	ContainerPort int
	// Volumes are the declared `volumes:` entries of an image: service, in the
	// docker `-v` form (named "name:/ctr" or host bind "/host:/ctr"). They are
	// passed verbatim to runtime.DeployService.Volumes; the launcher (the
	// space runtime) resolves each to an rw bind-mount. Required now that the
	// rootfs is read-only — see runtime/volumes.go.
	Volumes []string

	// RunnerRecipe, when non-nil, marks a run: service that must run on a BUILT
	// runner rootfs (baked system packages / toolchain) rather than the minimal
	// base + bind-mounted toolchain. The embedded backend builds the rootfs from
	// this recipe (runtime/runnerbuild) once per recipe hash and launches the
	// service on it. Other backends ignore it (they keep the bind-mount path).
	// Set by devServiceSpec when runnerimage.NeedsRunnerImage is true.
	RunnerRecipe *runnerimage.Runner
}

// ChooseDevBackend returns the embedded runc backend when the binary was built
// with -tags embedded_runtime AND the host can run it (Linux + rootless userns).
// ok=false means the caller MUST fall back to the legacy docker/process-host
// path — never a hard error, so `sklp dev` keeps working on a default build.
//
// The real implementation lives in embedded_on.go (tagged); embedded_off.go
// returns (nil, false) for every other build.
func ChooseDevBackend(spaceName string, allowHost bool) (devBackend, bool) {
	return chooseDevBackend(spaceName, allowHost)
}

// EmbeddedAvailable reports whether `sklp dev` will run its services in the
// embedded runc runtime (true) or fall back to docker/process-host (false).
// Mirrors the runner/spawn gating so the CLI can surface which mode is active.
func EmbeddedAvailable() bool { return embeddedAvailable() }

// defaultRepoMount is where the host repo is bind-mounted inside a run:
// service container for hot-reload. Mirrors the runner's /workspace so the
// in-container toolchain (skt) sees the source tree at a stable path.
const defaultRepoMount = "/workspace"

// defaultDevBaseImage is the MINIMAL glibc rootfs a run: service runs on: the
// space carries neither the app (it arrives by repo bind-mount) nor the
// toolchain (it arrives by skt-cache bind-mount). debian-slim is the smallest
// stock base that ships /bin/sh + glibc — required because node is
// dynamically linked (a musl base like alpine/busybox cannot run it).
// Overridable via SKLP_DEV_BASE_IMAGE.
const defaultDevBaseImage = "debian:trixie-slim"

// osGetenv is a thin seam so tests can override the toolchain image lookup
// without touching the process environment.
var osGetenv = os.Getenv

// devServiceSpec maps one NamedService into the backend-neutral DevServiceSpec.
// It encodes the dev policy:
//
//   - image: service  -> Image set, NO repo bind (deploy parity: an image:
//     service is a dependency like postgres, it must not see the host repo).
//   - run: service     -> toolchainImage + the shell command, repo bind-mounted
//     rw at /workspace for hot-reload, cwd at the service workdir under the
//     mount. toolchainImage is the skt-provisioned image (go/node/pnpm pinned
//     to the project versions, identical to CI).
//
// hostRepo is the absolute host repo root; subdir is the service's repo-relative
// workdir ("" = repo root). env is the already-composed KEY=value environment.
// It is pure (no FS/side effects) so the policy is unit-testable without the
// runc backend.
func devServiceSpec(ns NamedService, hostRepo string, tc devToolchain, env []string) DevServiceSpec {
	spec := DevServiceSpec{Name: ns.Name, Env: env}
	// Port-first: an explicit `port:` is the source of truth; absent it, the port
	// is parsed from an absolute `ready.http` URL (backward compatibility).
	var httpField string
	if ns.Def.Ready != nil {
		httpField = ns.Def.Ready.HTTP
	}
	if cp, ok := pipeline.PortFromReadyHTTP(ns.Def.Port, httpField); ok {
		spec.ContainerPort = cp
	}
	if ns.Def.Image != "" {
		spec.Image = ns.Def.Image
		spec.Command = ns.Def.Command
		// volumes: apply to image: services only (the schema enforces this).
		// They back the declared data paths now that the rootfs is read-only.
		spec.Volumes = ns.Def.Volumes
		return spec
	}
	// run: service — minimal glibc base, repo bind-mounted for hot-reload, the
	// host skt toolchain bind-mounted + on PATH. Neither the app nor the
	// toolchain lives in the image.
	spec.Image = devBaseImage()
	spec.ShellCmd = ns.Def.Run
	spec.HostRepo = hostRepo
	spec.RepoMount = defaultRepoMount
	spec.Workdir = ns.Def.Workdir
	spec.ConfigEnv = tc.Env
	spec.Secrets = tc.Secrets

	// If the project declares system packages, this run: service must run on a
	// BUILT runner rootfs (baked packages + toolchain) rather than the minimal
	// base + bind-mounted toolchain: the minimal base lacks those libs. Mark the
	// recipe; the embedded backend builds the rootfs and launches on it, and the
	// bind-mount of the host toolchain is then redundant (it's baked).
	if runnerimage.NeedsRunnerImage(tc.Recipe) {
		r := tc.Recipe
		spec.RunnerRecipe = &r
		return spec
	}

	spec.ToolchainBinDirs = tc.BinDirs
	spec.SktCacheMount = tc.CacheRoot
	return spec
}

// devBaseImage is the minimal glibc rootfs a run: service runs on. Overridable
// via SKLP_DEV_BASE_IMAGE for a project that needs a different base (e.g. extra
// system libs); the default is debian-slim. The toolchain is NOT in this image
// — it is bind-mounted from the host skt cache (see resolveDevToolchain).
func devBaseImage() string {
	if v := osGetenv("SKLP_DEV_BASE_IMAGE"); v != "" {
		return v
	}
	return defaultDevBaseImage
}
