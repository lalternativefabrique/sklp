// Package devstack runs `sklp dev`: a process supervisor for the
// long-running services declared in `.sklp/dev.yaml` (kind: devstack).
//
// The supervisor is intentionally stack-agnostic. It launches shell
// commands, optionally waits for an HTTP/TCP/log/file readiness probe,
// streams prefixed logs, and propagates SIGTERM/SIGKILL to the whole
// process group on shutdown. Anything that knows "this repo is Go"
// vs "this repo is Python" lives elsewhere (repofacts) and produces
// the YAML we consume here.
package devstack

import (
	"fmt"
	"sort"
	"time"

	"github.com/lalternativefabrique/sklp/pipeline"
)

// Stack is the validated, topologically-sorted set of services about
// to be launched. Build it with Load.
type Stack struct {
	// Name mirrors the YAML `name:` field, surfaced in startup banner.
	Name string

	// EnvFiles is the list of dotenv paths to load (repo-root-relative).
	EnvFiles []string

	// Env is the pipeline-level environment overlay applied to every
	// service before its own Env.
	Env map[string]string

	// Services is the launch order — already topologically sorted by
	// DependsOn. Each entry carries the pipeline.Service definition plus
	// its parsed Ready timeout.
	Services []NamedService

	// AllowHost mirrors the YAML `allowHost:` flag: let services reach the
	// host's own LAN addresses through the space egress blackhole. Dev-only —
	// see space.Space.AllowHostAccess for why deploy and CI have no equivalent.
	AllowHost bool
}

// NamedService pairs a service's YAML key with its definition. The
// supervisor uses Name as the log prefix.
type NamedService struct {
	Name         string
	Def          pipeline.Service
	ReadyTimeout time.Duration
}

// DefaultReadyTimeout is applied when a Service's Ready.Timeout is
// empty. Long enough to cover a cold `go run` compile on a slow box
// without making genuinely-stuck services hang forever.
const DefaultReadyTimeout = 30 * time.Second

// Load reads a devstack YAML, resolves `extends:`, validates the
// graph, and returns a launch-ready Stack. AutoFix=true (the default
// via LoadFile) lets the strict pre-pass rewrite high-confidence
// typos in place; tests pass false to keep fixtures pristine.
func Load(path string) (*Stack, error) {
	return LoadOpts(path, true)
}

// LoadOpts is Load with explicit auto-fix control.
func LoadOpts(path string, autoFix bool) (*Stack, error) {
	// Strict pre-pass: catch typos like `dependsOn` before the
	// permissive pipeline.LoadPipeline silently drops them. May rewrite
	// the file in place when a high-confidence fix exists, so a second
	// invocation reads the corrected source.
	if _, err := strictParse(path, autoFix); err != nil {
		return nil, err
	}
	pipe, err := pipeline.LoadPipeline(path)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", path, err)
	}
	if pipe.Kind != "devstack" {
		return nil, fmt.Errorf("%s: kind must be \"devstack\", got %q", path, pipe.Kind)
	}

	// Fold jobs into the service set as Oneshot services, the same shape deploy
	// uses (services + jobs -> one runnable set). A job's exit 0 is its ready
	// signal; the rest of the dev pipeline (depends_on redirect, topo sort,
	// launch, ready gating, crash watch) then treats it uniformly via Oneshot.
	// Done here, before any consumer reads pipe.Services, so nothing else needs
	// to know about jobs. Names must be unique across both families.
	if err := foldJobsIntoServices(&pipe); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	if len(pipe.Services) == 0 {
		return nil, fmt.Errorf("%s: services: must declare at least one service", path)
	}

	// Auto-redirect depends_on through oneshot intermediates BEFORE
	// topo-sorting. The rule: if a oneshot service M depends on X
	// (typically a migration step that runs against a freshly-booted
	// DB), any other long-running service that ALSO depends on X
	// implicitly depends on M completing first — otherwise the
	// backend would race the migration. We rewrite those edges in
	// place so the topo sort, the launch order, and the auto-injected
	// ready probes all see the corrected graph.
	//
	// Defends against the common LLM mistake of emitting `core:
	// depends_on: [infra]` when a `migrate` oneshot also exists. The
	// LLM follows the prompt 80% of the time; this guarantees the
	// remaining 20% don't ship broken stacks.
	redirectDependsOnThroughOneshot(pipe.Services)

	ordered, err := topoSort(pipe.Services)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	out := &Stack{
		Name:      pipe.Name,
		EnvFiles:  append([]string{}, pipe.EnvFiles...),
		Env:       pipe.Env,
		Services:  make([]NamedService, 0, len(ordered)),
		AllowHost: parseAllowHost(path),
	}
	// Auto-inject a generic log ready probe on every long-running
	// service that has at least one dependent and no explicit Ready.
	// Without this, a `depends_on:` chain would launch the dependent
	// the instant the upstream process forks — long before it actually
	// listens. The regex matches the most common "I'm up" line shapes
	// across compose containers, postgres, redis, nats, http servers,
	// etc. Users override by writing their own Ready block.
	//
	// Skipped for Oneshot services: they signal readiness via exit 0,
	// not via a probe.
	hasDependent := map[string]bool{}
	for _, svc := range pipe.Services {
		for _, dep := range svc.DependsOn {
			hasDependent[dep] = true
		}
	}

	for _, name := range ordered {
		svc := pipe.Services[name]
		// A service is either a shell command (`run:`) or an OCI
		// container (`image:`) — exactly one. Surface both error
		// shapes here so the user gets a precise message instead of
		// "run: is required" when they meant image.
		switch {
		case svc.Run == "" && svc.Image == "":
			return nil, fmt.Errorf("%s: service %q: either run: or image: is required", path, name)
		case svc.Run != "" && svc.Image != "":
			return nil, fmt.Errorf("%s: service %q: run: and image: are mutually exclusive", path, name)
		}
		// volumes/command stay image-only on a `run:` service: command is the
		// image entrypoint override (a run: already IS its command), and a
		// run:'s data lives in the bind-mounted repo, not a named volume.
		// ports: ARE allowed on a run: — in the embedded runtime a run: service
		// is a container in the space, so a dev server (vite, a go http server)
		// must publish its port through the ingress to be reachable from the
		// host browser, exactly like an image: service. (Under the in-process
		// fallback the process listens on the host directly; a declared port is
		// then just the probe/ingress hint, harmless.)
		if svc.Run != "" {
			if len(svc.Volumes) > 0 || len(svc.Command) > 0 {
				return nil, fmt.Errorf("%s: service %q: volumes/command only apply to image: services", path, name)
			}
		}
		// Configuration sanity: Oneshot + Ready makes no sense — exit
		// is the only ready signal for a one-shot. We surface this
		// loudly so the user fixes the YAML rather than wondering why
		// their probe is ignored.
		if svc.Oneshot && svc.Ready != nil {
			return nil, fmt.Errorf("%s: service %q: oneshot services cannot declare ready (exit 0 IS the ready signal)", path, name)
		}
		if !svc.Oneshot && svc.Ready == nil && hasDependent[name] {
			svc.Ready = &pipeline.Ready{
				Log:     defaultLogReadyPattern,
				Timeout: defaultLogReadyTimeout,
			}
		}
		// Port-first: resolve `ready.http` once, here, where the service still
		// carries both Port and Ready. A bare path (`/health`) is composed onto
		// `http://localhost:<port>`; an absolute URL passes through. Every
		// downstream probe consumer then reads an already-dialable URL.
		if svc.Ready != nil && svc.Ready.HTTP != "" {
			resolved := pipeline.ComposeReadyHTTP(svc.Ready.HTTP, svc.Port)
			if resolved == "" {
				return nil, fmt.Errorf("%s: service %q: ready.http %q is a bare path but no port: is declared — add `port:` so the probe URL can be composed", path, name, svc.Ready.HTTP)
			}
			svc.Ready.HTTP = resolved
		}
		timeout := DefaultReadyTimeout
		if svc.Ready != nil && svc.Ready.Timeout != "" {
			d, err := time.ParseDuration(svc.Ready.Timeout)
			if err != nil {
				return nil, fmt.Errorf("%s: service %q: ready.timeout %q: %w", path, name, svc.Ready.Timeout, err)
			}
			timeout = d
		}
		out.Services = append(out.Services, NamedService{
			Name:         name,
			Def:          svc,
			ReadyTimeout: timeout,
		})
	}
	return out, nil
}

// defaultLogReadyPattern matches the genuine "I'm accepting traffic"
// log lines for the services people typically front their dev stacks
// with: HTTP servers, NATS, Postgres, MySQL, Redis, MongoDB, etc.
//
// We deliberately do NOT match generic words like `started` or
// `ready` on their own because docker compose itself emits
// "Container <name> Started" the instant the container's pid is
// alive — long before Postgres / NATS finish their own boot. That
// false positive bit bakery on the first run: `make migrate` ran
// against a postgres container that was up but still initialising.
//
// Each alternation below is a high-confidence "the service is ready
// to accept work" marker drawn from the canonical log of the tool:
//
//   - Postgres        : "database system is ready to accept connections"
//   - MySQL/MariaDB   : "ready for connections"
//   - Redis           : "Ready to accept connections"
//   - MongoDB         : "Waiting for connections"
//   - NATS            : "Server is ready"
//   - HTTP frameworks : "listening on"
//   - generic Go HTTP : "http server started" / "listening on :<port>"
//
// Adding a new service usually means appending one alternation here —
// keep it specific. The user can always override with an explicit
// `ready: { log: ... }` block per service.
// Each alternation is a verified "I'm now accepting work" marker. We
// deliberately AVOID generic words like `listening on` because
// Postgres logs "listening on IPv6 address ..." mid-startup, BEFORE
// it accepts queries (it's still doing crash recovery). The real
// readiness line is `database system is ready to accept connections`.
// Same logic for every entry below: pick the line emitted AFTER the
// service is actually serving, not just bound to the port.
const defaultLogReadyPattern = `(?i)(database system is ready to accept connections|ready for connections|ready to accept connections|waiting for connections|nats server is ready|server is ready)`

// foldJobsIntoServices merges pipe.Jobs into pipe.Services, marking each job
// Oneshot=true. It is the dev/pipeline analogue of deploy's runnables(): jobs
// and services become one set, jobs distinguished only by Oneshot. Names must
// be unique across both families (a job and a service cannot share a name), and
// a job MUST NOT declare ready (exit 0 is the signal — same rule the validator
// enforces for Oneshot). After this, pipe.Jobs is cleared so no consumer reads
// it twice.
func foldJobsIntoServices(pipe *pipeline.Pipeline) error {
	if len(pipe.Jobs) == 0 {
		return nil
	}
	if pipe.Services == nil {
		pipe.Services = map[string]pipeline.Service{}
	}
	for name, job := range pipe.Jobs {
		if _, clash := pipe.Services[name]; clash {
			return fmt.Errorf("name %q declared as both a service and a job (names must be unique across both)", name)
		}
		if job.Ready != nil {
			return fmt.Errorf("job %q cannot declare ready (a job's exit 0 IS its ready signal)", name)
		}
		job.Oneshot = true
		pipe.Services[name] = job
	}
	pipe.Jobs = nil
	return nil
}

// redirectDependsOnThroughOneshot rewrites edges so any non-oneshot
// service that depends on X is upgraded to depend on a oneshot M
// when M itself depends on X. The map is mutated in place; the
// caller re-reads from it after this function returns.
//
// Algorithm (single pass, O(S² · D) — fine since S+D are tiny):
//
//	for each oneshot M:
//	  let upstream = M.DependsOn
//	  for each other non-oneshot service S:
//	    if S.DependsOn ∩ upstream is non-empty AND M ∉ S.DependsOn:
//	      replace each upstream-overlap edge in S.DependsOn with M
//
// We DO NOT chain through multiple oneshots in the same pass to keep
// the rule predictable: a oneshot M2 that itself depends on M1 won't
// silently turn S→M1 into S→M2. That's the user's call (they can add
// explicit edges).
func redirectDependsOnThroughOneshot(services map[string]pipeline.Service) {
	for mName, m := range services {
		if !m.Oneshot {
			continue
		}
		upstream := map[string]bool{}
		for _, dep := range m.DependsOn {
			upstream[dep] = true
		}
		if len(upstream) == 0 {
			continue
		}
		for sName, s := range services {
			if sName == mName || s.Oneshot {
				continue
			}
			alreadyDependsOnM := false
			for _, dep := range s.DependsOn {
				if dep == mName {
					alreadyDependsOnM = true
					break
				}
			}
			if alreadyDependsOnM {
				continue
			}
			rewrote := false
			for i, dep := range s.DependsOn {
				if upstream[dep] {
					s.DependsOn[i] = mName
					rewrote = true
				}
			}
			if rewrote {
				services[sName] = s
			}
		}
	}
}

// defaultLogReadyTimeout caps the auto-injected probe. Long enough
// to cover docker compose pulling images on a cold cache; short
// enough to fail fast on a misconfigured service.
const defaultLogReadyTimeout = "60s"

// topoSort returns service names in dependency order (deps first).
// Cycles or unknown depends_on targets are reported as errors. Ties
// are broken alphabetically so the launch order is deterministic
// across runs (helpful for log diffing).
func topoSort(services map[string]pipeline.Service) ([]string, error) {
	// Validate depends_on targets up front: a typo in a dep name
	// would otherwise silently behave like "no dep" under Kahn's
	// algorithm.
	for name, svc := range services {
		for _, dep := range svc.DependsOn {
			if _, ok := services[dep]; !ok {
				return nil, fmt.Errorf("service %q depends_on unknown service %q", name, dep)
			}
			if dep == name {
				return nil, fmt.Errorf("service %q depends_on itself", name)
			}
		}
	}

	indeg := make(map[string]int, len(services))
	for name, svc := range services {
		indeg[name] = len(svc.DependsOn)
	}

	// Kahn's algorithm with alphabetical tie-break for determinism.
	var ready []string
	for name, d := range indeg {
		if d == 0 {
			ready = append(ready, name)
		}
	}
	sort.Strings(ready)

	var out []string
	for len(ready) > 0 {
		name := ready[0]
		ready = ready[1:]
		out = append(out, name)
		// Find services that depended on `name` and decrement.
		var newlyReady []string
		for other, svc := range services {
			for _, dep := range svc.DependsOn {
				if dep == name {
					indeg[other]--
					if indeg[other] == 0 {
						newlyReady = append(newlyReady, other)
					}
				}
			}
		}
		sort.Strings(newlyReady)
		ready = append(ready, newlyReady...)
	}

	if len(out) != len(services) {
		// Cycle: some node never reached indeg==0. Name the survivors
		// so the user can spot the loop.
		var stuck []string
		for name, d := range indeg {
			if d > 0 {
				stuck = append(stuck, name)
			}
		}
		sort.Strings(stuck)
		return nil, fmt.Errorf("depends_on cycle among services: %v", stuck)
	}
	return out, nil
}
