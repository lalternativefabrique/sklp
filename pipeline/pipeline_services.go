// Package runner — local-services orchestration for `sklp run`.
//
// This file is the runtime counterpart of PR #369 (which added K8s
// sidecars). When a pipeline is executed OUTSIDE Kubernetes — i.e.
// directly on a developer laptop — every service declared in
// `pipeline.services:` and referenced by at least one step must be
// brought up locally before the first dependent step runs.
//
// In practice every real `sklp run` either executes in-process on the
// host (which never declares image services that need a supervisor) or
// inside a Space / K8s Pod, where services are managed externally as
// netns-sharing sidecars (see servicesManagedExternally). The
// StartPipelineServicesHook below is the extension point for an in-host
// orchestrator; it is nil by default (no wiring after the docker/podman
// CI engine was removed), and RunPipeline is nil-safe — when nil the
// executor logs a notice and skips, exactly as on K8s.
//
// # K8s skip contract
//
// The K8s spawner (the server's k8s spawner) sets
// SKLP_RUNNER_MODE=k8s on the runner container's env. When that env
// var is observed here, the orchestrator is bypassed: services are
// already running as sidecars in the Pod, and an extra local boot
// would either fail (no docker in the container) or, worse, race the
// real services.
package pipeline

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/lalternativefabrique/sklp/pipeline/spawn"
)

// RunnerModeEnv is the env var that tells the executor it is running
// inside a Kubernetes Pod started by K8sJobSpawner. When set to "k8s"
// the local services orchestrator is bypassed.
//
// Kept exported so the spawner and tests can refer to the same name.
const RunnerModeEnv = "SKLP_RUNNER_MODE"

// RunnerModeK8s is the value of SKLP_RUNNER_MODE set by the K8s
// spawner. Any other value (or unset) means "local execution" — the
// orchestrator runs.
const RunnerModeK8s = "k8s"

// RunnerModeEmbedded is the value of SKLP_RUNNER_MODE set by the embedded
// OCI backend (space). Like K8s, services are managed OUTSIDE
// the runner container — the runtime launches them as netns-sharing
// sidecars host-side, so the inner runner must NOT boot them locally
// (there is no docker in the runner image). The shared netns makes them
// reachable on localhost:<port>, exactly like a K8s Pod.
const RunnerModeEmbedded = "embedded-oci"

// StartPipelineServicesFunc is the signature of the local-services
// orchestrator. It boots every service in `requested` (already
// filtered to the union of names referenced by at least one step),
// waits for each one to reach Ready, and returns a teardown closure
// plus an optional port map (legacy local-bridge path).
//
// The teardown is idempotent and always safe to call (including on
// partial boot failure) — the executor wires it as a `defer`.
//
//   - ctx is the pipeline-level context. Cancellation triggers
//     teardown.
//   - requested maps service name → spec.
//   - envOverlay is the pipeline-level Env (already merged with any
//     env_files by the caller, if applicable).
//   - workdir is the repo root, used as the default cwd for shell
//     services and the lookup root for service.Workdir.
type StartPipelineServicesFunc func(
	ctx context.Context,
	requested map[string]Service,
	envOverlay map[string]string,
	workdir string,
	stdout, stderr io.Writer,
) (teardown func(), portEnv map[string]string, err error)

// StartPipelineServicesHook is the registration point for an in-host
// services orchestrator. It is nil by default (no production wiring
// since the docker/podman CI engine was removed); RunPipeline is
// nil-safe and logs "[sklp] services orchestrator not registered —
// skipping" when it is unset. Kept as an extension point that
// runner-only unit tests also populate.
var StartPipelineServicesHook StartPipelineServicesFunc

// requestedServices returns the subset of p.Services referenced by at
// least one in-scope step's `services:` list. Steps gated out at
// compile time (scope filter) are excluded — booting postgres + nats
// for a backend-only step makes no sense when the diff is 100%
// frontend and the step is going to be skipped anyway.
//
// Pass nil for includedSteps to opt out of filtering. An empty
// (non-nil) map means "no step is in scope" and yields nil.
//
// Steps that reference an unknown service name are silently ignored
// — LoadPipeline already enforces the cross-reference; we trust it.
func requestedServices(p Pipeline, includedSteps map[string]bool) map[string]Service {
	if len(p.Services) == 0 {
		return nil
	}
	used := map[string]struct{}{}
	for _, st := range p.Steps {
		if includedSteps != nil && !includedSteps[st.Name] {
			continue
		}
		for _, name := range st.Services {
			used[name] = struct{}{}
		}
	}
	if len(used) == 0 {
		return nil
	}
	out := make(map[string]Service, len(used))
	for name := range used {
		if svc, ok := p.Services[name]; ok {
			out[name] = svc
		}
	}
	return out
}

// sortedServiceNames returns the keys of m sorted alphabetically.
// Stable order keeps the startup log line deterministic across runs.
func sortedServiceNames(m map[string]Service) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// servicesManagedExternally reports whether the runner is executing under
// a spawner that already brought up the services as netns-sharing
// sidecars (K8s Pod or embedded OCI runtime). In both modes the local
// boot is skipped; services are addressed by name on the bridge.
func servicesManagedExternally() bool {
	switch os.Getenv(RunnerModeEnv) {
	case RunnerModeK8s, RunnerModeEmbedded:
		return true
	default:
		return false
	}
}

// startServicesForPipeline is the small wrapper RunPipeline calls.
// Centralises the decisions (skip on K8s, skip on no requested
// services, skip when hook unregistered) so the executor body stays
// straight-line readable.
//
// Returns a no-op teardown when nothing was started — the caller can
// always `defer teardown()` without a nil check.
func startServicesForPipeline(
	ctx context.Context,
	p Pipeline,
	includedSteps map[string]bool,
	repoPath string,
	stdout, stderr io.Writer,
) (func(), map[string]string, error) {
	requested := requestedServices(p, includedSteps)
	if len(requested) == 0 {
		return func() {}, nil, nil
	}
	if servicesManagedExternally() {
		fmt.Fprintf(stderr, "[sklp] %s=%s — services managed externally (netns sidecars): [%s]\n",
			RunnerModeEnv, os.Getenv(RunnerModeEnv),
			joinSorted(sortedServiceNames(requested)))
		// Sidecars are already up; we don't boot anything and publish no port
		// map — step env addresses each service as <name>:<port> verbatim.
		return func() {}, nil, nil
	}
	if StartPipelineServicesHook == nil {
		fmt.Fprintf(stderr, "[sklp] services orchestrator not registered — skipping local boot of: [%s]\n",
			joinSorted(sortedServiceNames(requested)))
		return func() {}, nil, nil
	}
	fmt.Fprintf(stderr, "[sklp] starting %d service(s) locally: [%s]\n",
		len(requested), joinSorted(sortedServiceNames(requested)))
	return StartPipelineServicesHook(ctx, requested, p.Env, repoPath, stdout, stderr)
}

// pipelineServicesForEmbedded maps the pipeline's requested services into
// the neutral spawn.Service slice the embedded runtime consumes. It runs
// HOST-SIDE (before the container is spawned), so it has no includedSteps
// scope filter: it takes the union of every step's `services:` — a
// documented super-set of what a scoped run would touch. Optional
// services are dropped (they are opt-in via a future --with flag).
//
// Returns nil when the pipeline declares no requested services, which
// keeps the runtime on its byte-identical single-container path.
//
// Per-service Env is layered over the pipeline-level Env (service wins),
// mirroring the schema's documented precedence.
func pipelineServicesForEmbedded(p Pipeline) []spawn.Service {
	requested := requestedServices(p, nil)
	if len(requested) == 0 {
		return nil
	}
	var out []spawn.Service
	for _, name := range sortedServiceNames(requested) {
		svc := requested[name]
		if svc.Optional {
			continue
		}
		out = append(out, spawn.Service{
			Name:      name,
			Image:     svc.Image,
			Run:       svc.Run,
			Command:   svc.Command,
			Env:       mergeServiceEnv(p.Env, svc.Env),
			DependsOn: svc.DependsOn,
			Oneshot:   svc.Oneshot,
			Ready:     readyForEmbedded(svc.Ready, svc.Port),
			Ports:     svc.Ports,
			Volumes:   svc.Volumes,
			Network:   svc.Network,
		})
	}
	return out
}

// mergeServiceEnv layers a service's Env over the pipeline-level Env. The
// service entry wins on key collision. Returns nil when both are empty.
func mergeServiceEnv(pipelineEnv, serviceEnv map[string]string) map[string]string {
	if len(pipelineEnv) == 0 && len(serviceEnv) == 0 {
		return nil
	}
	out := make(map[string]string, len(pipelineEnv)+len(serviceEnv))
	for k, v := range pipelineEnv {
		out[k] = v
	}
	for k, v := range serviceEnv {
		out[k] = v
	}
	return out
}

// readyForEmbedded maps a schema.Ready into the neutral spawn.ServiceReady
// (nil stays nil = launch-and-forget). port is the service's declared container
// port, used to compose a bare-path `ready.http` (`/health`) into a dialable URL
// (port-first contract); an absolute URL passes through unchanged.
func readyForEmbedded(r *Ready, port int) *spawn.ServiceReady {
	if r == nil {
		return nil
	}
	return &spawn.ServiceReady{
		HTTP:    ComposeReadyHTTP(r.HTTP, port),
		TCP:     r.TCP,
		Log:     r.Log,
		File:    r.File,
		Timeout: r.Timeout,
	}
}

// joinSorted joins names with a single space. Tiny helper kept local
// so we don't pull strings.Join into a file that otherwise has no
// need for the import.
func joinSorted(names []string) string {
	out := ""
	for i, n := range names {
		if i > 0 {
			out += " "
		}
		out += n
	}
	return out
}
