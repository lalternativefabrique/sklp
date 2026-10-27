package spawn

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/lalternativefabrique/sklp/space"
)

// embeddedRunnerModeEnv is the SKLP_RUNNER_MODE entry the embedded backend
// injects so the inner runner skips its local services boot (no docker in
// the image). MUST equal runner.RunnerModeEmbedded; spawn cannot import
// runner (cycle), so the value is duplicated and asserted in tests.
const embeddedRunnerModeEnv = "SKLP_RUNNER_MODE=embedded-oci"

// defaultPidsLimit caps processes/threads per run as a fork-bomb backstop.
// Generous enough for real builds (compilers, test runners, pnpm) yet far
// below what exhausts the host PID space. The userns is shared with the
// host, so an uncapped fork bomb would hit the host's pid_max.
const defaultPidsLimit = 4096

// ciCPUsEnv overrides the per-run CPU budget (whole cores).
const ciCPUsEnv = "SKLP_CI_CPUS"

// ciCPUBudget is the cores one CI run may use: SKLP_CI_CPUS, else a quarter
// of the host (at least 2) so three concurrent spaces leave the host
// responsive. It is both the cgroup cpu.max cap (best-effort — dropped where
// cgroups v2 are not delegated) and the SKLP_CPUS budget the inner runner
// derives its step and GOMAXPROCS parallelism from, which needs no cgroup.
func ciCPUBudget() int {
	if n, err := strconv.Atoi(os.Getenv(ciCPUsEnv)); err == nil && n > 0 {
		return n
	}
	return defaultCICPUs(runtime.NumCPU())
}

func defaultCICPUs(hostCPUs int) int {
	n := hostCPUs / 4
	if n < 2 {
		n = 2
	}
	if n > hostCPUs {
		n = hostCPUs
	}
	return n
}

// inCI reports whether we're running in a CI context, where a requested
// resource cap must be enforced (RequireLimits) rather than silently
// dropped. Honours the conventional CI=true plus an explicit SKLP_CI.
func inCI() bool {
	return os.Getenv("SKLP_CI") != "" || os.Getenv("CI") == "true" || os.Getenv("CI") == "1"
}

// spawnEmbedded routes a Spawn() call to the Space (daemonless runc)
// backend. The translation is mostly a 1:1 field rename, plus a
// best-effort .env load when EnvFile is set (parsed here and passed
// through as Env, since the Space takes no env-file flag).
func (s *OCISpawner) spawnEmbedded(ctx context.Context, cfg Config) (string, error) {
	image, err := s.resolveImage(cfg.RunnerKind)
	if err != nil {
		return "", quoteErr(s.Name(), err)
	}
	mountSrc, err := s.hostMountFor(cfg.RepoPath)
	if err != nil {
		return "", quoteErr(s.Name(), err)
	}

	cpus := ciCPUBudget()
	env := []string{
		"SKLP_CPUS=" + strconv.Itoa(cpus),
		"SKLP_PROVIDER=" + cfg.Provider.Kind,
		"SKLP_MODEL=" + cfg.Provider.Model,
	}
	if cfg.Provider.BaseURL != "" {
		env = append(env, "SKLP_BASE_URL="+cfg.Provider.BaseURL)
	}
	if cfg.Provider.APIKey != "" {
		for _, name := range apiKeyEnvNames(cfg.Provider.Kind) {
			env = append(env, name+"="+cfg.Provider.APIKey)
		}
	}
	for _, name := range sortedKeys(cfg.Secrets) {
		env = append(env, name+"="+cfg.Secrets[name])
	}
	if cfg.EnvFile != "" {
		extra, err := loadDotEnv(cfg.EnvFile)
		if err != nil {
			return "", quoteErr(s.Name(), fmt.Errorf("env-file %s: %w", cfg.EnvFile, err))
		}
		env = append(env, extra...)
	}

	// Signal the inner runner that services are managed host-side (by this
	// runtime) so it does NOT try to boot them locally (no docker in the
	// image). Same contract as SKLP_RUNNER_MODE=k8s for sidecars.
	env = append(env, embeddedRunnerModeEnv)

	services, err := mapEmbeddedServices(cfg.Services, cfg.LiveOut)
	if err != nil {
		return "", quoteErr(s.Name(), err)
	}

	// A pids cap is the cheap anti-fork-bomb default: the pids controller is
	// delegated even on hosts that withhold cpu/memory, so it almost always
	// applies. In CI (CI=true / SKLP_CI=1) a requested cap is a contract —
	// RequireLimits makes the run FAIL rather than silently run unbounded if
	// cgroups v2 delegation is missing, so one leaky run can't OOM the host.
	report, err := space.Run(ctx, space.Config{
		Image:         image,
		RepoPath:      mountSrc,
		TaskName:      cfg.TaskName,
		ExtraArgs:     cfg.ExtraArgs,
		Env:           env,
		LiveOut:       cfg.LiveOut,
		Services:      services,
		CPULimit:      float64(cpus),
		PidsLimit:     defaultPidsLimit,
		RequireLimits: inCI(),
	})
	if err != nil {
		// Errors from the embedded package already carry the
		// "spawner(embedded): ..." prefix, so we don't double-wrap.
		return report, err
	}
	return report, nil
}

// mapEmbeddedServices translates the neutral spawn.Service slice into the
// runtime package's Service shape via space.ServiceFromSchema. It is the
// ONLY place the gated runtime package is referenced for services, so the
// untagged build never sees it.
//
// It rejects shell (`run:`) services and image-less services with an
// actionable error (the embedded backend only runs `image:` services),
// and emits a single grouped warning when any service uses bridge
// networking or declares ports — the shared-netns model ignores both.
func mapEmbeddedServices(in []Service, liveOut io.Writer) ([]space.Service, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]space.Service, 0, len(in))
	for _, svc := range in {
		if svc.Image == "" && svc.Run == "" {
			return nil, fmt.Errorf("service %q has neither image: nor run:; the Space runtime needs one", svc.Name)
		}
		// A `run:` service has no image of its own: it executes on the space's
		// runner rootfs (which bakes the space.yaml toolchain). The Space path
		// routes it there; see runViaSpace.
		out = append(out, space.ServiceFromSchema(
			svc.Name,
			svc.Image,
			svc.Run,
			svc.Command,
			envSliceFromMap(svc.Env),
			svc.DependsOn,
			svc.Oneshot,
			readyToRuntime(svc.Ready),
			svc.Ports,
			svc.Volumes,
			svc.Network,
		))
	}
	return out, nil
}

// readyToRuntime maps the neutral ServiceReady into space.ServiceReady.
func readyToRuntime(r *ServiceReady) *space.ServiceReady {
	if r == nil {
		return nil
	}
	return &space.ServiceReady{
		HTTP:    r.HTTP,
		TCP:     r.TCP,
		Log:     r.Log,
		File:    r.File,
		Timeout: r.Timeout,
	}
}

// envSliceFromMap renders a KEY=value slice from a map, sorted by key so
// the spec is deterministic. Returns nil for an empty map.
func envSliceFromMap(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}

// loadDotEnv parses a KEY=value file (the docker --env-file subset:
// comments allowed, no quoting, no exports) into a slice the embedded
// runtime can hand to the OCI spec. Kept tiny on purpose; full
// dotenv semantics aren't part of the docker --env-file contract.
func loadDotEnv(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		out = append(out, line)
	}
	return out, nil
}
