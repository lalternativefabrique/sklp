package devstack

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/lalternativefabrique/sklp/pipeline"
)

// ociRuntime is the OCI engine sklp drives as a subprocess. We don't
// link any client SDK — argv composition + exec is enough for v1 and
// keeps the dependency tree light.
type ociRuntime struct {
	// Name of the binary on PATH ("docker", "podman", "nerdctl").
	// Surfaced in logs so the user knows which engine is doing what.
	Name string
}

// detectOCIRuntime walks the preferred-runtime list and returns the
// first one available on PATH. Order rationale:
//
//   - docker: most common on Linux/Mac dev machines, drives compose
//     too so users are likely to have it.
//   - podman: rootless / no daemon — preferred when present, but we
//     don't ALWAYS pick it because some users have podman installed
//     as a docker-cli compatibility layer they don't actively use.
//   - nerdctl: containerd's CLI; smallest install footprint.
//
// Returns an error when none are found. The supervisor surfaces this
// as a launch error for the affected service only — other shell
// services keep working.
func detectOCIRuntime() (*ociRuntime, error) {
	for _, name := range []string{"docker", "podman", "nerdctl"} {
		if _, err := exec.LookPath(name); err == nil {
			return &ociRuntime{Name: name}, nil
		}
	}
	return nil, fmt.Errorf("no OCI runtime found on PATH (looked for: docker, podman, nerdctl)")
}

// ociContainerName builds the container name used for the lifecycle
// commands. We prefix with `sklp-dev-` so users can `docker ps | grep
// sklp-dev` to find everything sklp launched, and we include the
// service name so two `sklp dev` runs in different projects don't
// collide.
//
// Note: the prefix is fixed across runs, so re-running `sklp dev`
// reuses the same name. The runtime's `--rm` plus our pre-launch
// `<rt> rm -f` (in stop()) cover the "previous run crashed and left
// a container behind" case.
func ociContainerName(svcName string) string {
	return "sklp-dev-" + svcName
}

// buildOCIArgv composes the argv for `<runtime> run` from a Service.
// We use `--rm` so the container is wiped on stop, and `--name` so
// our stop hook can target it by name without grep-ing `docker ps`.
//
// Network semantics depend on `mode`:
//
//   - NetworkHost: legacy. `--network host` shares the host namespace
//     so the container reaches the world at `localhost:<port>`. Kept
//     for one release cycle while in-repo dev.yaml files migrate.
//   - NetworkBridge: `--network <netName>` attaches the container to
//     the run-scoped bridge. The container is also given `--hostname
//     <svc>` so peers resolve it east-west by service name. Host ports
//     are NOT published — port templating was removed; the embedded runc
//     backend's hostname mux is the supported host-exposure path.
//
// envOverlay lets the caller pre-merge pipeline env + envFiles +
// service env before this function picks up the result. Keeps argv
// composition pure (no side-effects on the OS environment).
func buildOCIArgv(
	rt *ociRuntime,
	svc pipeline.Service,
	name string,
	envOverlay map[string]string,
	mode NetworkMode,
	netName string,
) []string {
	argv := []string{rt.Name, "run", "--rm", "--name", ociContainerName(name)}

	switch mode {
	case NetworkBridge:
		argv = append(argv, "--network", netName, "--hostname", name)
	default:
		// Host network: matches compose `network_mode: host`.
		argv = append(argv, "--network", "host")
	}

	// Env: sorted for deterministic argv (helps test diffs and log
	// readability). The image's own ENV stays in effect except for
	// keys we explicitly override.
	if len(envOverlay) > 0 {
		keys := make([]string, 0, len(envOverlay))
		for k := range envOverlay {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			argv = append(argv, "-e", k+"="+envOverlay[k])
		}
	}

	// Volumes: bind or named. We don't normalise host paths here —
	// the runtime resolves them relative to its own cwd (the user's
	// cwd when they run sklp dev), which matches what `docker run`
	// would do directly.
	for _, v := range svc.Volumes {
		argv = append(argv, "-v", v)
	}

	argv = append(argv, svc.Image)
	argv = append(argv, svc.Command...)
	return argv
}

// ociStopArgv returns the argv to gracefully stop a container by
// name. We use `stop` (SIGTERM with 10s grace) over `kill` so the
// container's PID 1 can flush state. The supervisor's own grace
// window handles the SIGKILL fallback if needed.
func ociStopArgv(rt *ociRuntime, svcName string) []string {
	return []string{rt.Name, "stop", ociContainerName(svcName)}
}

// ociPreLaunchCleanupArgv removes a stale container with our name
// before we try to launch a fresh one. Used when a previous `sklp
// dev` was killed -9 and left a container behind (the `--rm` flag
// didn't get a chance to fire).
//
// `rm -f` is destructive but acceptable here: the target is a
// container we control by naming convention (sklp-dev-<svc>), and
// the only failure mode is "container didn't exist" which we treat
// as success.
func ociPreLaunchCleanupArgv(rt *ociRuntime, svcName string) []string {
	return []string{rt.Name, "rm", "-f", ociContainerName(svcName)}
}

// runOCICleanup executes the pre-launch cleanup synchronously,
// swallowing the inevitable "no such container" error. Returns only
// genuine runtime errors (e.g. docker daemon down).
func runOCICleanup(ctx context.Context, rt *ociRuntime, svcName string) error {
	cmd := exec.CommandContext(ctx, "sh", "-c",
		// Quote the binary + name to handle exotic characters; the
		// service name is user-controlled YAML.
		ociShellCommand(ociPreLaunchCleanupArgv(rt, svcName))+
			" >/dev/null 2>&1 || true")
	return cmd.Run()
}

// ociShellCommand quotes an argv into a single shell-safe string.
// We embed each arg in single quotes and escape inner single quotes
// via the standard '\” dance. Used to feed our normal sh -c
// machinery without introducing a new exec path.
func ociShellCommand(argv []string) string {
	var b strings.Builder
	for i, a := range argv {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteByte('\'')
		b.WriteString(strings.ReplaceAll(a, `'`, `'\''`))
		b.WriteByte('\'')
	}
	return b.String()
}
