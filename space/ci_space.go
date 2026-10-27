package space

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lalternativefabrique/sklp/internal/stackconfig"
	"github.com/lalternativefabrique/sklp/space/host"
)

// runViaSpace runs a CI pipeline through a single runtime.Space instead of the
// hand-rolled shared-netns orchestrate.go. It is the convergence of the CI
// runtime onto the same engine deploy/dev use:
//
//   - each service is a Space service (Space.Start) → SpaceNet bridge with
//     DNS-by-name (a step reaches postgres at "postgres:5432", not 127.0.0.1)
//     and egress wired by the createRuntime hook from INSIDE the netns (the
//     mechanism that works on a rootless runc netns, unlike host-side nsenter);
//   - the runner is a parked Space service with the repo bind-mounted at
//     /workspace; `sklp run <task>` is then exec'd into it via RuncExecStreams,
//     capturing stdout (the JSON report) exactly like the legacy runRunner.
//
// The space is scoped per task AND per repo checkout, then torn down (Close) on
// return, success or failure. The step executor (internal/plan) is unchanged:
// it runs inside the runner container as before.
func runViaSpace(ctx context.Context, cfg Config) (string, error) {
	space, err := NewSpace(ciSpaceName(cfg.TaskName, cfg.RepoPath))
	if err != nil {
		return "", fmt.Errorf("spawner(embedded): space: %w", err)
	}
	if err := requireSpaceHost(space.CacheRoot(), cfg.LiveOut); err != nil {
		return "", err
	}
	// Close tears down containers, network and ingress but deliberately keeps
	// per-space state on disk — right for a deployment, wrong for CI, whose
	// space is recreated from scratch every run. Without this the spaces/ tree
	// grows one never-reclaimed directory per task × checkout.
	//
	// Defers unwind LIFO, so this pair MUST stay in this order: Close reaps the
	// containers by listing `runc --root <spaceDir>/runc-state`, which
	// removeCISpaceState deletes. Reclaiming the state first blinds the reap and
	// strands every service process in a cgroup runc then refuses to reuse.
	defer removeCISpaceState(space)
	defer space.Close()
	// Deferred rather than tail-called: a failed run leaves a bucket behind
	// exactly like a passing one. Reclaim failures are not the pipeline's
	// verdict, so they never alter the result.
	defer collectToolcacheAfterRun(cfg.LiveOut)
	// Declared BEFORE the rootfs unlock below so LIFO runs it AFTER: a collector
	// that sees this run's own pin still held would skip the very entry the
	// budget most needs counted, and the LRU pass would evict an older one
	// instead.
	defer collectRunnerAfterRun(cfg.LiveOut)

	// The runner rootfs is BUILT on the fly from .sklp/space.yaml (base + apt +
	// toolchain + skt + the current sklp), cached by recipe hash — never a pulled
	// runner image, so the tools always match the repo. It backs both the runner
	// container AND any image-less `run:` service, so a `run:` service (e.g.
	// migrate) sees the same baked tools.
	const runnerName = "runner"
	buildOut, closeBuildOut := liveOutFile(cfg.LiveOut)
	runnerRootfs, bakedEnv, _, unlockRootfs, err := buildSpaceRootfs(ctx, space, cfg.RepoPath, "", buildOut)
	closeBuildOut()
	// Held until every container mounting this rootfs is torn down, so a
	// concurrent `sklp cache gc` cannot unlink it mid-run.
	defer unlockRootfs()
	if err != nil {
		return "", fmt.Errorf("spawner(embedded): build space rootfs: %w", err)
	}

	// Services first: each becomes a Space service on the bridge, reachable by
	// name. Readiness is gated by Space.Start's probe wiring (Ready carried on
	// the DeployService), so a dependent — and ultimately the runner — only
	// proceeds once its upstreams answer.
	// The runner rootfs is read-only and its recipe may set HOME=/root, so every
	// tool that derives a cache from $HOME (sklp, go's GOPATH/GOMODCACHE/GOCACHE,
	// node/pnpm) would try to write under /root and abort. The bundle's
	// tmpfs /tmp is only 2g — too small for a real build's $WORK + go-build
	// cache + module cache (a full `go test ./...` overflows it with "no space
	// left on device"). Two cache tiers:
	//   /cache-shared — GOMODCACHE/GOPATH: content-addressable, concurrent-read
	//                   safe; shared across all tasks to avoid N-fold duplication.
	//   /cache        — GOCACHE/GOTMPDIR + HOME: writer-sensitive per-task state.
	sharedCacheDir := filepath.Join(space.CacheRoot(), "ci-toolcache", "shared")
	// Keyed like the space itself (task + checkout digest): HOME/GOCACHE/GOTMPDIR
	// are writer-sensitive, so two worktrees running the same task must not share
	// them. GOMODCACHE stays in /cache-shared — content-addressed, safe to share.
	taskCacheDir := filepath.Join(space.CacheRoot(), "ci-toolcache", ciSpaceName(cfg.TaskName, cfg.RepoPath))
	// Pre-create the dirs the env points at: go refuses to mkdir GOTMPDIR, and a
	// bind source must exist. The container root maps to the host uid, so private
	// owner-only dirs are writable without making cache state world-writable.
	for _, sub := range []string{"go/pkg/mod"} {
		if err := ensurePrivateDir(filepath.Join(sharedCacheDir, sub)); err != nil {
			return "", fmt.Errorf("spawner(embedded): shared cache dir: %w", err)
		}
	}
	for _, sub := range []string{"go-tmp", ".cache/go-build"} {
		if err := ensurePrivateDir(filepath.Join(taskCacheDir, sub)); err != nil {
			return "", fmt.Errorf("spawner(embedded): task cache dir: %w", err)
		}
	}
	// The bucket name only carries a truncated, non-invertible digest of the
	// checkout path, so record the path itself: it is what lets the collector
	// tell a dead checkout's cache from one it merely failed to enumerate.
	if err := writeToolcacheOrigin(taskCacheDir, cfg.RepoPath, cfg.TaskName, time.Now()); err != nil {
		return "", fmt.Errorf("spawner(embedded): task cache origin: %w", err)
	}
	// Held for the whole run: this bucket is about to be bind-mounted into the
	// runner, and a concurrent collection pass must not unlink it mid-build.
	defer lockToolcacheBucket(taskCacheDir)()
	ordered, err := orderServices(cfg.Services)
	if err != nil {
		return "", fmt.Errorf("spawner(embedded): services: %w", err)
	}

	// On the bridge a service listens on its DECLARED port at its own name (no
	// host-port mapping), so a ci.yaml addresses it as "<name>:<port>" verbatim —
	// SpaceNet DNS resolves <name> to the bridge IP. No string templating.
	for _, svc := range ordered {
		ds := DeployService{
			Name:    svc.Name,
			Image:   svc.Image,
			Command: svc.Command,
			Env:     svc.Env,
			Volumes: svc.Volumes,
		}
		if svc.Run != "" {
			// Image-less shell service: the project itself, run on the built space
			// rootfs (baked toolchain) with the checkout at /workspace and the same
			// writable caches the runner gets, or `cd apps/core && go run .` finds
			// no apps/core. The run: command is used verbatim — it addresses
			// upstreams as <name>:<port>.
			ds.Image = ""
			ds.RootfsDir = runnerRootfs
			ds.RootfsCfg = ImageConfig{Env: bakedEnv}
			ds.IgnoreEntrypoint = true
			ds.Command = []string{"sh", "-c", svc.Run}
			ds.Env = append(append([]string{}, writableCacheEnv()...), svc.Env...)
			ds.WorkdirOverride = "/workspace"
			ds.DevMounts = []DevMount{
				{Source: cfg.RepoPath, Destination: "/workspace"},
				{Source: sharedCacheDir, Destination: "/cache-shared"},
				{Source: taskCacheDir, Destination: "/cache"},
			}
		}
		if _, err := space.Start(ctx, ds); err != nil {
			return "", fmt.Errorf("spawner(embedded): service %q: %w", svc.Name, err)
		}
		if err := WaitServiceReady(ctx, svc.Name, svc.Ready, space.ProbeCoords(svc.Name)); err != nil {
			return "", fmt.Errorf("spawner(embedded): service %q: %w", svc.Name, err)
		}
	}

	// The runner: a parked container with the repo at /workspace, on the same
	// bridge as the services. We park its init on a long sleep and exec the real
	// `sklp run` into it so we capture stdout (the report) — Space.Start launches
	// detached and does not return the process stdout.
	// The inner `sklp run` resolves step env (e.g. DATABASE_URL) itself; it
	// addresses services as <name>:<port> on the bridge, no port map injected.
	// Layer env: baked recipe env (PATH/toolchain) first, then cfg.Env (carries
	// SKLP_RUNNER_MODE), then the writable-cache redirects — later entries win.
	runnerEnv := append([]string{}, bakedEnv...)
	runnerEnv = append(runnerEnv, cfg.Env...)
	runnerEnv = append(runnerEnv, secretsPassphraseEnv(cfg.RepoPath)...)
	runnerEnv = append(runnerEnv, writableCacheEnv()...)
	// The inner `sklp run` writes its per-run log to the CENTRAL store, which it
	// resolves inside the container from XDG_CACHE_HOME (/cache/.cache) and
	// -repo (/workspace) → /cache/.cache/sklp/logs/workspace. Bind the HOST
	// central dir for the REAL repo onto that path so the log lands on the host
	// keyed by the real repo (where the cockpit's LatestCILogForRepo looks),
	// not buried in the task cache under a "workspace" slug.
	hostLogDir := centralLogDir(cfg.RepoPath)
	containerLogDir := filepath.Join("/cache/.cache", "sklp", "logs", "workspace")
	if err := os.MkdirAll(hostLogDir, 0o755); err != nil {
		return "", fmt.Errorf("spawner(embedded): central log dir: %w", err)
	}
	runner := DeployService{
		Name:             runnerName,
		RootfsDir:        runnerRootfs,
		RootfsCfg:        ImageConfig{Env: bakedEnv},
		IgnoreEntrypoint: true,
		NestedRuntime:    true,
		PauseInit:        true, // park init; real work execs in via `sklp run`
		Resources:        cfg.limits(),
		RequireResources: cfg.RequireLimits,

		Env:             runnerEnv,
		WorkdirOverride: "/workspace",
		DevMounts: []DevMount{
			{Source: cfg.RepoPath, Destination: "/workspace"},
			{Source: sharedCacheDir, Destination: "/cache-shared"},
			{Source: taskCacheDir, Destination: "/cache"},
			{Source: hostLogDir, Destination: containerLogDir},
		},
	}
	// When /workspace is a linked git worktree, its `.git` file points at the
	// main repo's .git/worktrees/<name>/ by absolute host path — outside
	// /workspace, so the bind above does not bring it in and every `git` step
	// (e.g. branch-context) dies with exit 128 "not a git repository". Bind the
	// main repo at that same absolute path (ro) so git resolves the pointer.
	if main := mainRepoForWorktree(cfg.RepoPath); main != "" {
		runner.DevMounts = append(runner.DevMounts,
			DevMount{Source: main, Destination: main, ReadOnly: true})
	}
	if _, err := space.Start(ctx, runner); err != nil {
		return "", fmt.Errorf("spawner(embedded): start runner: %w", err)
	}

	taskName := cfg.TaskName
	if taskName == "" {
		taskName = "ci"
	}
	argv := append([]string{"sklp", "run", taskName, "-repo", "/workspace", "-output", "json"}, cfg.ExtraArgs...)

	// `runc exec` does not inherit the init env, so pass everything the inner
	// runner needs explicitly: cfg.Env (carries SKLP_RUNNER_MODE=embedded-oci so
	// it skips local service boot) and the writable-cache redirects. Same set as
	// the parked init's Env (runnerEnv).
	execEnv := runnerEnv

	runcBin, runcRoot, id := space.ExecCoords(runnerName)
	var report strings.Builder
	code, err := RuncExecStreamsEnv(ctx, runcBin, runcRoot, id, execEnv, argv, nil, &report, cfg.LiveOut)
	out := strings.TrimSpace(report.String())
	if err != nil {
		return out, fmt.Errorf("spawner(embedded): runner exec: %w", err)
	}
	if code != 0 {
		return out, fmt.Errorf("spawner(embedded): runner exited %d", code)
	}
	return out, nil
}

// secretsPassphraseEnv forwards the secrets passphrase into the runner. The
// inner `sklp run` reads .sklp/**/config.yaml itself, and that file is useless
// without the passphrase — without this, a repo migrated off dotenv fails the
// run before its first step with "passphrase is empty".
//
// It resolves rather than reading the env alone: the vault is the whole point
// of not exporting the passphrase in every shell, and the runner has no vault
// of its own — no keychain, no D-Bus session, none of the host's sklp config.
// Resolving on the host and carrying the value is the same move limarelay
// makes for the VM guest.
//
// Nothing else carries it: the sandbox allowlist deliberately withholds ambient
// host credentials from steps, and this is the container boundary, not a step.
// Absent everywhere, nothing is forwarded — a repo still on env_files is
// unaffected, and one declaring no secrets must not be asked for a key.
func secretsPassphraseEnv(repoPath string) []string {
	v, _, err := stackconfig.ResolvePassphrase(repoPath)
	if err != nil || v == "" {
		return nil
	}
	return []string{stackconfig.PassphraseEnv + "=" + v}
}

// writableCacheEnv redirects every tool cache off the read-only rootfs.
// Two mount points are in play (see runViaSpace):
//
//	/cache-shared — GOPATH/GOMODCACHE: content-addressable, shared across tasks.
//	/cache        — GOCACHE/GOTMPDIR + HOME: per-task, writer-sensitive.
func writableCacheEnv() []string {
	return []string{
		"HOME=/cache",
		"XDG_CACHE_HOME=/cache/.cache",
		"XDG_DATA_HOME=/cache/.local/share",
		"XDG_CONFIG_HOME=/cache/.config",
		"SKLP_TOOLS_CACHE=/cache/sklp-tools",
		"GOPATH=/cache-shared/go",
		"GOMODCACHE=/cache-shared/go/pkg/mod",
		"GOCACHE=/cache/.cache/go-build",
		"GOTMPDIR=/cache/go-tmp",
		"npm_config_cache=/cache/.npm",
		"PNPM_HOME=/cache/.pnpm",
	}
}

// ciSpaceName is the space identity for a CI run: the task name plus a short
// digest of the repo checkout it runs against. Two worktrees of the same repo
// running the same task therefore get DISJOINT spaces — separate bridge,
// separate runc state root, separate id prefix (invariant I4) — so their
// services (postgres, nats) never share state or reap each other's containers.
// Keying on the checkout path rather than the branch keeps the space stable
// across a checkout inside one worktree, and lets consecutive runs in that
// worktree reuse the space.
func ciSpaceName(taskName, repoPath string) string {
	base := sanitize(taskName)
	if strings.TrimSpace(repoPath) == "" {
		// No checkout to key on. filepath.Abs("") would silently return the
		// PROCESS cwd, making the space identity depend on where sklp was
		// invoked from; fall back to the shared per-task space instead.
		return base
	}
	abs, err := filepath.Abs(repoPath)
	if err != nil {
		return base
	}
	sum := sha256.Sum256([]byte(abs))
	return base + "-" + hex.EncodeToString(sum[:4])
}

// envKeyify uppercases a service name and collapses non-[A-Z0-9] runs to a
// single "_", trimming edges — used to derive a stable per-service key (e.g.
// anonymous volume directory names).
func envKeyify(name string) string {
	var b strings.Builder
	prevUnder := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 32)
			prevUnder = false
		case (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevUnder = false
		default:
			if !prevUnder {
				b.WriteByte('_')
				prevUnder = true
			}
		}
	}
	return strings.Trim(b.String(), "_")
}

// requireSpaceHost refuses to start a Space on a host that cannot run one,
// naming every gap, instead of letting each one surface later as an
// unrelated failure (a mute egress, a step that cannot mount /proc).
func requireSpaceHost(cacheRoot string, out io.Writer) error {
	problems := host.Check(cacheRoot)
	for _, p := range problems {
		if !p.Fatal && out != nil {
			fmt.Fprintf(out, "[sklp] host: %s\n", p)
		}
	}
	if !host.HasFatal(problems) {
		return nil
	}
	var b strings.Builder
	b.WriteString("spawner(embedded): this host cannot run a Space (sklp host check):")
	for _, p := range problems {
		if p.Fatal {
			b.WriteString("\n  ")
			b.WriteString(p.String())
		}
	}
	return errors.New(b.String())
}

// liveOutFile turns the run's live writer into a file the rootfs build can
// hand to its child processes, and returns the function that drains it. A nil
// writer keeps the build on stderr.
func liveOutFile(w io.Writer) (*os.File, func()) {
	if w == nil {
		return nil, func() {}
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, func() {}
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(w, pr)
		pr.Close()
		close(done)
	}()
	return pw, func() {
		pw.Close()
		<-done
	}
}
