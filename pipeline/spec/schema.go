// Package runner — pipeline schema (v1).
//
// This file defines the declarative schema sklp contracts use: the
// product-facing `steps:` contract.
//
//	version: 1
//	kind: pipeline
//	extends: _base
//	runner: base
//	tools:    { go: "1.25.0" }
//	env:      { CI: "true" }
//	secrets:  [ GITHUB_TOKEN ]
//	cache:    [ { path: ~/.cache/go-build, key: go-build } ]
//	steps:
//	  - name: test
//	    run: go test ./...
//
// LoadPipeline reads a YAML file, resolves `extends:`, and returns a
// fully merged Pipeline.
package spec

//go:generate go run ../../cmd/genschema

// Pipeline is the resolved shape of a `.sklp/tasks/<name>.yaml` file
// after `extends:` has been merged. Field order mirrors the YAML user
// flow: identity, composition, environment, work.
type Pipeline struct {
	// Version pins the schema; 1 is the only valid value today.
	Version int `yaml:"version"`

	// Kind selects the executor: "pipeline" (runs Steps) or "devstack".
	// The legacy "task" (runtask) executor has been removed; a contract
	// that is not kind: pipeline is rejected by Run with an explicit
	// error. Empty is treated as a non-pipeline kind (and therefore
	// rejected) — author contracts as kind: pipeline. ("publish" is a
	// step Kind, not a pipeline Kind — see Step.Kind.)
	Kind string `yaml:"kind,omitempty"`

	// Space, when set to false, disables the Space sandbox: the pipeline runs
	// directly on the host instead of inside a Space container. Use for
	// pipelines that need host-level access (e.g. publish steps with runc).
	Space *bool `yaml:"space,omitempty"`

	// Extends names other files in the same .sklp/tasks/ directory whose
	// content should be merged into this Pipeline before execution.
	// Accepts a single string or a list of strings (resolved by the
	// custom YAML unmarshaller below).
	Extends ExtendsList `yaml:"extends,omitempty"`

	// On declares which branches a push may select this pipeline on.
	//
	// This answers a different question from require.branches. That one is a
	// guard: where the pipeline is ALLOWED to run at all, checked at run time
	// and refusing the run when it does not hold. This one is a trigger: where
	// a push should START it on its own. publish is allowed to run only on
	// main and must never be started by a push, so it needs to say both — one
	// field cannot carry the two.
	//
	// Omitted means "every branch", which is what an unrestricted CI pipeline
	// wants. An explicit empty Except list is still "every branch"; a pipeline
	// that must never be selected declares `on: {never: true}` and is started
	// by name only.
	On *TriggerBlock `yaml:"on,omitempty"`

	// Runner selects the container image used by DockerSpawner. Empty
	// means "base". Recognised values are "base", "ops", or any full
	// image reference starting with the canonical registry root.
	Runner string `yaml:"runner,omitempty"`

	// Tools maps tool name -> exact version, overriding or extending the
	// runner config's `runner.toolchain` (config.yaml). The effective
	// toolchain a run sees is the UNION of the two: config defines the
	// execution env, the pipeline may pin a different version or add a
	// tool. A pipeline entry with an empty version inherits the config
	// pin. Installed by sklp before the first Step runs. Resolution
	// order: native registry, then skt. Versions must be semver
	// or a documented skt tag; "latest" / "lts" are rejected.
	Tools map[string]string `yaml:"tools,omitempty"`

	// Env adds non-secret environment variables to every Step. Values
	// are plain strings; templating is not interpreted at parse time.
	Env map[string]string `yaml:"env,omitempty"`

	// Secrets lists logical secret names that must exist in the
	// project's secret store. The runner resolves them at spawn time
	// and mounts them at /run/secrets/<NAME> + exports them as env vars.
	Secrets []string `yaml:"secrets,omitempty"`

	// Registries declares one push credential per registry host, in the
	// shape a deploy.yaml uses. Read only on a CI node (SKLP_CI / CI=true),
	// where no interactive `sklp registry login` exists: the password is
	// written as ${VAR} and resolved from the run's environment. A dev
	// machine ignores the block and pushes with its own login.
	Registries map[string]RegistryCredential `yaml:"registries,omitempty"`

	// Cache declares directories whose content is persisted between
	// runs of the same project. The key may contain `{{ checksum
	// "path" }}` templating; cache miss = empty volume, cache hit =
	// previous run's content.
	Cache []CacheEntry `yaml:"cache,omitempty"`

	// Scopes maps a logical name to a list of doublestar globs. A step
	// referencing a scope (via Step.Scope) only runs when at least one
	// changed file in the current run matches one of the scope's globs.
	//
	// Declared once at the pipeline level, reused by N steps. The user
	// expresses the dependency between "this part of the codebase" and
	// "these steps" instead of duplicating glob lists step by step.
	//
	// A pipeline without `scopes:` has no skip logic — every step runs.
	// That matches mono-service repos where every PR touches the
	// (single) service by definition.
	Scopes map[string][]string `yaml:"scopes,omitempty"`

	// Steps are the ordered shell commands to execute. v1 is sequential
	// with no fan-out; matrix and parallel come later behind their own
	// fields.
	Steps []Step `yaml:"steps,omitempty"`

	// EnvFiles lists dotenv-style files loaded into the runner's
	// process environment before anything else runs. For `kind:
	// pipeline`, values are visible to require: evaluation, tag:
	// resolution, step env composition, and
	// BuildKit secret resolution. For `kind: devstack`, files also feed
	// each Service's env layer before the per-service Env overlay.
	//
	// Paths are repo-root-relative. Missing files are silently skipped
	// (a `.env.local` that doesn't exist is fine). Within the list,
	// last file wins on key collision. Host env always wins over file
	// content — a CI runner that explicitly sets GITHUB_TOKEN is never
	// overridden by a checked-in `.env`.
	// 427: env_files for kind: pipeline
	EnvFiles []string `yaml:"env_files,omitempty"`

	// Services declares the long-running processes managed by
	// `sklp dev`. Only consulted when Kind == "devstack". Map key is
	// the service name surfaced in the prefixed log mux ([api ], …).
	Services map[string]Service `yaml:"services,omitempty"`

	// Jobs declares run-once-to-completion tasks (migration, seed, one-off
	// setup) managed by `sklp dev`, a SEPARATE typed family from Services
	// (mirrors deploy's services/jobs split, k8s Deployment vs Job). A job is
	// folded into the service set with Oneshot=true: exit 0 IS its ready signal,
	// dependents gate on its completion, and the supervisor never treats its
	// exit as a crash. Only consulted when Kind == "devstack"; names must be
	// unique across Services and Jobs.
	Jobs map[string]Service `yaml:"jobs,omitempty"`

	// 413: tag block ---------------------------------------------------
	//
	// Tag, when set, declares a deterministic publish-tag strategy. The
	// resolved tag is computed ONCE at compile time, exported to every
	// step as $SKLP_PUBLISH_TAG, and substituted into `{{ publish_tag }}`
	// placeholders in step Run and Env values. When Tag is nil, the
	// pipeline behaves exactly as before: no env var, no substitution.
	Tag *TagBlock `yaml:"tag,omitempty"`
	// 413: end tag block -----------------------------------------------

	// ---- publish-contract fields ----
	//
	// publish.yaml uses these; the deterministic resolver reads them via
	// runner/resolve.go's `contract` struct. Re-declared here so the
	// extends-merge engine round-trips them losslessly and a single
	// LoadPipeline call returns the complete shape. `Tasks` is kept for
	// lossless YAML round-tripping only — no executor consumes it since
	// the legacy runtask path was removed.

	Name        string           `yaml:"name,omitempty"`
	Description string           `yaml:"description,omitempty"`
	Tasks       []map[string]any `yaml:"tasks,omitempty"`
	Inputs      map[string]any   `yaml:"inputs,omitempty"`
	Targets     map[string]any   `yaml:"targets,omitempty"`
	ServiceMap  map[string]any   `yaml:"service_map,omitempty"`
	Defaults    map[string]any   `yaml:"defaults,omitempty"`
	Rules       []string         `yaml:"rules,omitempty"`

	// 412: require block — declarative preconditions evaluated by the
	// engine BEFORE any step compiles or runs. Omit entirely to keep
	// today's default (no preconditions). See RequireBlock for fields.
	Require *RequireBlock `yaml:"require,omitempty"`
}

// RequireBlock declares pipeline-level preconditions. The engine
// evaluates each non-empty field before compiling any step; a single
// failed check refuses the run with a clear message and no side
// effects (no Docker spin-up, no service start, no tool resolution).
//
// Opt-in: omit the block entirely to disable every check. Future
// extension points (signed_commit, head_unpublished, …) follow the
// same pattern — new optional field, evaluated at the same point.
type RequireBlock struct {
	// Branches lists git branch names allowed to run this pipeline.
	// Empty / omitted = no branch restriction. The check shells out
	// to `git rev-parse --abbrev-ref HEAD` against RepoPath.
	Branches []string `yaml:"branches,omitempty"`

	// CleanTree, when true, refuses the run if `git status --porcelain`
	// returns any output (unstaged changes OR untracked files).
	CleanTree bool `yaml:"clean_tree,omitempty"`
}

// RegistryCredential is one host's entry in a pipeline `registries:` block.
type RegistryCredential struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// TriggerBlock declares where a pipeline may run. The same list answers
// both questions a run asks — whether a push selects it, and whether a
// run named by hand is allowed to proceed — because splitting them across
// two blocks let a pipeline be guarded on one path and open on the other.
type TriggerBlock struct {
	// Branches lists glob patterns matched against the current branch,
	// in order, with a "!" prefix negating. Later patterns win, so
	// ['**', '!main'] reads as "every branch except main".
	//
	// It gates selection AND execution: a push on a branch matching no
	// pattern does not select the pipeline, and `sklp run <name>` from
	// that same branch is refused rather than silently running.
	//
	// An empty or omitted list means every branch. A pipeline that must
	// never be selected on its own declares the branches it is allowed
	// to run on and nothing else — publish says [main], so a merge
	// cannot start it while a hand-run from main still works.
	Branches []string `yaml:"branches,omitempty"`

	// CleanTree, when true, refuses the run if `git status --porcelain`
	// reports anything (unstaged changes or untracked files). An image
	// built from uncommitted work is reproducible from no commit.
	CleanTree bool `yaml:"clean_tree,omitempty"`

	// Except is the pre-branches spelling of a negated pattern list.
	// Deprecated: write branches: ['**', '!main'].
	Except []string `yaml:"except,omitempty"`

	// Never is the pre-branches spelling of "not selectable by a push".
	// Deprecated: write the branches the pipeline may run on instead.
	Never bool `yaml:"never,omitempty"`
}

// Step is one shell command (or block) inside a pipeline.
type Step struct {
	// Name is a human label surfaced in the run log. Required.
	Name string `yaml:"name"`

	// Kind selects the step shape. Empty (the default) means a plain
	// shell step driven by Run. "publish" is a first-class wrapper that
	// builds an image, scans it with Trivy, and pushes it as one
	// cacheable unit — fully daemonless (no Docker); the Image / Tag /
	// Context / Dockerfile / Push / Scan fields configure it. Adding new
	// kinds requires bumping plan.SchemaVersion so old cached plans
	// cannot be misinterpreted.
	Kind string `yaml:"kind,omitempty"`

	// Image is the registry reference produced by a `kind: publish`
	// step (without the tag). Ignored for plain shell steps.
	Image string `yaml:"image,omitempty"`

	// Tag is the image tag for a `kind: publish` step. Literal
	// strings or `$VAR` env interpolations only — the full template
	// engine (issue #413) is not wired yet. Ignored for shell steps.
	Tag string `yaml:"tag,omitempty"`

	// Context is the build context directory, relative to the
	// repo root. Required for `kind: publish`. Ignored otherwise.
	Context string `yaml:"context,omitempty"`

	// Dockerfile is the path to the Dockerfile, relative to the repo
	// root. Defaults to `<context>/Dockerfile`. Ignored for shell steps.
	Dockerfile string `yaml:"dockerfile,omitempty"`

	// Push is the opt-out switch for the push phase of a publish
	// step. Modeled as a pointer so YAML omission is distinguishable
	// from an explicit `push: false` — the default is push: true.
	Push *bool `yaml:"push,omitempty"`

	// Scan configures the Trivy scan phase. Modeled as RawNode so the
	// YAML may be either `scan: false` (full opt-out), an absent key
	// (defaults applied: severity HIGH/CRITICAL, ignore_unfixed true),
	// or a `scan: { severity: [...], ignore_unfixed: bool }` mapping.
	Scan ScanConfigYAML `yaml:"scan,omitempty"`

	// BuildArgs lists `--build-arg KEY=VALUE` entries for a
	// `kind: publish` step. Values pass through the pipeline
	// template engine (env vars + `{{ publish_tag }}`-style
	// placeholders) before reaching the builder. Non-secret only —
	// build args end up in the image's history; use BuildSecrets for
	// anything sensitive. Ignored for non-publish steps.
	BuildArgs map[string]string `yaml:"build_args,omitempty"`

	// BuildContexts maps a BuildKit additional-context name to a source
	// reference, one per `--build-context <name>=<value>` entry the
	// builder honours. The source may be:
	//
	//   - a local directory (relative to the build context root or
	//     absolute) — its contents are folded into the step's cache key
	//   - `docker-image://<ref>` — the image ref (incl. tag/digest) is
	//     folded verbatim into the cache key
	//   - `target:<stage>` — a named stage in the same Dockerfile, no
	//     cache impact beyond the Dockerfile already being hashed
	//   - a git URL (`https://...git#ref`) or OCI layout — ref folded
	//     verbatim
	//
	// The value is treated as opaque by sklp: the builder parses it and
	// returns a clean error on a bad ref. Ignored for non-publish
	// steps. (Issue #445.)
	BuildContexts map[string]string `yaml:"build_contexts,omitempty"`

	// Platform is the os/arch a `kind: publish` step builds for
	// (`linux/amd64` or `linux/arm64`). The base image is pulled for
	// that platform, the image is labelled with it, and RUN steps of a
	// foreign platform execute through the host's binfmt qemu-user
	// handler, as `docker build --platform` does. Defaults to the
	// build host's platform. Ignored for non-publish steps.
	Platform string `yaml:"platform,omitempty"`

	// Run is the shell snippet executed via `sh -c`. Multi-line YAML
	// scalars are supported and forwarded verbatim. Required for the
	// default (shell) step kind; OPTIONAL for `kind: publish`,
	// which is driven by Image / Dockerfile / Context instead. The
	// validator that catches "shell step without run" lives in the
	// pipeline compiler — the JSON schema stays permissive to allow
	// non-shell kinds, so the `,omitempty` tag is on purpose.
	Run string `yaml:"run,omitempty"`

	// Scope names one or more entries from Pipeline.Scopes. When set,
	// the step runs only if at least one file changed in the current
	// run matches one of the named scopes' globs. Empty = the step
	// always runs.
	//
	// Multiple scopes form an OR: `scope: [a, b]` runs when a OR b
	// has changed files. The custom unmarshaler in schema_scope.go
	// accepts both scalar and list YAML forms.
	Scope ScopeRef `yaml:"scope,omitempty"`

	// If is NOT supported. Conditional step execution is intentionally
	// not a feature: a non-empty value is rejected at compile time with a
	// message pointing at the alternative (a shell test inside `run:`).
	// The field is kept in the schema only so the rejection can name it
	// precisely instead of failing as an unknown key. Do not gate steps
	// with `if:` — gate behaviour from within the command.
	If string `yaml:"if,omitempty"`

	// Env merges into the pipeline-level Env for this step only.
	Env map[string]string `yaml:"env,omitempty"`

	// Tools, when set, restricts the tools resolved for this step to
	// the listed subset of the effective toolchain (pipeline `tools:`
	// merged with config `runner.toolchain`). When unset, the step
	// inherits every tool in that effective toolchain (the full env).
	// Each entry must be a key of the effective toolchain, otherwise
	// the executor fails with a clear error before running the step.
	//
	// Combined with scope gates, this is what makes tool installs
	// lazy: a backend-only PR never resolves node/pnpm/bun because the
	// frontend steps that need them are skipped before their tools are
	// touched.
	Tools []string `yaml:"tools,omitempty"`

	// DependsOn lists names of other steps in the same pipeline that
	// MUST complete successfully before this step runs. When unset on
	// every step in a pipeline, the compiler treats the declared order
	// as an implicit linear chain. When at least one step declares it,
	// the compiler builds an explicit DAG: any step without DependsOn
	// is a root, ordering between independent steps is lexicographic.
	DependsOn []string `yaml:"depends_on,omitempty"`

	// Cache opts a step OUT of the step cache. Modeled as a pointer so
	// YAML omission is distinguishable from an explicit `cache: false`
	// — the default is cache: true.
	//
	// Set it false on a step whose result is NOT a function of its
	// declared inputs because it mutates state living outside the repo:
	// a migration that resets a live database, a fixture loader, a step
	// that publishes to an external system. Replaying such a step from
	// the cache skips the mutation while reporting success, so the next
	// step runs against state the cache silently assumed was there.
	//
	// Pure steps (compile, lint, test) must stay cacheable — this is an
	// escape hatch for external effects, not a way to force a re-run.
	Cache *bool `yaml:"cache,omitempty"`

	// Cwd is the working directory the step runs in, relative to the
	// repo root. Empty means run at the repo root. The path is cleaned
	// and rejected at compile time if it escapes the repo root.
	Cwd string `yaml:"cwd,omitempty"`

	// Network selects the network isolation policy for this step. The
	// only recognised non-empty value today is "none", which asks the
	// in-process sandbox to unshare the network namespace on Linux. On
	// macOS the field is accepted and logged as a no-op.
	Network string `yaml:"network,omitempty"`

	// Filesystem selects the filesystem isolation policy for this step.
	// The default (empty) is "confined": the step may read/write the
	// repo, /tmp and its secret dir, read system/toolchain paths, and
	// everything else (notably $HOME — ~/.ssh, ~/.aws, …) is denied via
	// Landlock on Linux. The only recognised non-empty value is "open",
	// which disables confinement (host-wide FS access). On macOS or on
	// kernels without Landlock the field is accepted and the step runs
	// without FS confinement (logged), never failing for that reason.
	Filesystem string `yaml:"filesystem,omitempty"`

	// Secrets restricts which of Pipeline.Secrets are exposed to this
	// step (shell step shape), OR declares BuildKit `--secret id=...`
	// entries for a `kind: publish` step. The custom YAML
	// unmarshaller (schema_publish.go) accepts:
	//
	//   secrets: [ FOO, BAR ]                       # shell step
	//   secrets:                                    # publish
	//     - id: github_token
	//       env: GITHUB_TOKEN
	//     - id: npmrc
	//       file: ~/.npmrc
	//
	// nil means "no secret exposed" — secrets are opt-in per step to
	// keep the blast radius of any single `run:` snippet small.
	Secrets StepSecrets `yaml:"secrets,omitempty"`

	// Inputs lists doublestar globs (relative to repo root) whose
	// content is folded into this step's cache key. Editing a file
	// matched by a glob invalidates the step (and its descendants);
	// editing a file outside every step's inputs does not. Empty =
	// the step's key ignores the working tree entirely.
	Inputs []string `yaml:"inputs,omitempty"`

	// Outputs lists literal paths (relative to repo root, NOT globs)
	// the step produces. On a successful run they are copied into the
	// repo-local content-addressed store; on a cache hit they are
	// restored from the store before downstream steps run. Empty =
	// no output capture.
	Outputs []string `yaml:"outputs,omitempty"`

	// Services lists the names of Pipeline.Services this step requires.
	// Each entry must match a key in Pipeline.Services (validated at
	// load time). When the step runs in a K8s pod, the named services
	// are materialised as sidecar containers in the same Pod so the
	// step can reach them on localhost:<port>.
	//
	// This field is currently ignored by every backend except K8s: a
	// log line is emitted listing the requested service names so the
	// operator can see the intent, but no container is started. K8s
	// is the only backend that injects sidecars in v1.
	Services []string `yaml:"services,omitempty"`
}

// Service is one long-running process managed by `sklp dev`. The
// supervisor launches it inside its own process group, optionally
// waits for a Ready probe to pass, and propagates SIGTERM/SIGKILL on
// shutdown. Per-service fields layer over the pipeline-level Env and
// EnvFiles.
type Service struct {
	// Run is the shell command executed via `sh -c`. Mutually
	// exclusive with Image: a service is EITHER a shell command (run)
	// OR a container (image).
	Run string `yaml:"run,omitempty"`

	// Image, when set, marks this service as an OCI container managed
	// natively by sklp (via docker / podman / nerdctl auto-detected on
	// PATH). Mutually exclusive with Run. The full reference is
	// passed verbatim to the runtime — `postgres:16-alpine`,
	// `rg.fr-par.scw.cloud/skaplai/foo:v1.2.3`, etc.
	Image string `yaml:"image,omitempty"`

	// Port is the container port the service listens on — the single source
	// of truth. It drives host-mux exposure AND lets `ready.http` be written
	// as a bare path (`/health`), composed onto `http://localhost:<port>`.
	// When unset, the port is inferred from an absolute `ready.http` URL for
	// backward compatibility. Declaring `port:` is the supported form.
	Port int `yaml:"port,omitempty"`

	// Ports maps host:container or :container (auto host port). Only
	// honoured when Image is set. Examples: `5432:5432`, `:8080`.
	Ports []string `yaml:"ports,omitempty"`

	// Volumes lists named or bind mounts. Only honoured with Image.
	// Examples: `pg-data:/var/lib/postgresql/data`, `./data:/data:ro`.
	Volumes []string `yaml:"volumes,omitempty"`

	// Command overrides the image's default CMD. Slice form so
	// arguments with spaces don't get re-split. Only with Image.
	Command []string `yaml:"command,omitempty"`

	// Workdir is the directory the command runs in, relative to the
	// repo root. Empty = repo root. Ignored for Image services.
	Workdir string `yaml:"workdir,omitempty"`

	// Env overlays on top of the pipeline-level Env + EnvFiles for
	// this service only.
	Env map[string]string `yaml:"env,omitempty"`

	// DependsOn names other services that must reach Ready before
	// this one is launched. Cycles are rejected at validation time.
	DependsOn []string `yaml:"depends_on,omitempty"`

	// Optional services are skipped by default; opt-in via a future
	// `--with <name>` flag. Lets `dev.yaml` declare the full stack
	// (e.g. an OTEL collector) without forcing every contributor to
	// run it.
	Optional bool `yaml:"optional,omitempty"`

	// Oneshot marks a process expected to run to completion (exit 0)
	// before its dependents start, rather than staying alive for the
	// whole stack lifetime. Typical use: schema migrations
	// (`go run . migrate up`, `prisma migrate deploy`, `alembic
	// upgrade head`). Exit 0 = "ready" (= "done") and unblocks
	// `depends_on` chains; exit ≠ 0 fails the whole stack.
	//
	// Oneshot services ignore Ready: they signal readiness via their
	// exit, not via a probe.
	Oneshot bool `yaml:"oneshot,omitempty"`

	// Ready describes how the supervisor knows the service is up.
	// Empty = launch-and-forget (no gating for downstream services).
	Ready *Ready `yaml:"ready,omitempty"`

	// Network selects the network namespace for an `image:` service:
	//
	//   - "bridge" — the supervisor attaches the container to a
	//     run-scoped bridge network created on demand (`sklp-dev-<id>`
	//     or `sklp-ci-<id>`). Services are reached by their name as DNS
	//     on the bridge (`postgres:5432`); steps address them by
	//     name:port, never via a host-port env map. Required for running
	//     multiple `sklp dev` / `sklp run ci` invocations in parallel on
	//     the same host.
	//   - "host" — legacy: the container shares the host network
	//     namespace (`--network host`). `ports:` are ignored. Hard-
	//     codes port numbers and prevents parallel runs on the same
	//     host. Emits a deprecation warning when selected (or when no
	//     value is provided and the supervisor falls back to host for
	//     compatibility); will be removed once in-repo dev.yaml files
	//     have migrated to bridge.
	//
	// Empty means: defer to the supervisor's current default (host for
	// now, bridge after the migration PR lands). Ignored for `run:`
	// services.
	Network string `yaml:"network,omitempty"`
}

// Ready is the readiness probe for a Service. Exactly one of HTTP /
// TCP / Log / File should be set; the supervisor picks the first
// non-empty one in that order and ignores the rest.
type Ready struct {
	// HTTP is a URL polled with GET; any 2xx response means ready.
	HTTP string `yaml:"http,omitempty"`

	// TCP is a host:port string opened with net.DialTimeout.
	TCP string `yaml:"tcp,omitempty"`

	// Log is a regex matched line-by-line against the service's
	// stdout/stderr. First match = ready.
	Log string `yaml:"log,omitempty"`

	// File is a path that must exist on disk (Unix socket, pid file).
	File string `yaml:"file,omitempty"`

	// Timeout caps the wait. Default 30s applied by the supervisor
	// when zero. Format: Go duration string ("30s", "2m").
	Timeout string `yaml:"timeout,omitempty"`
}

// CacheEntry describes one persisted directory. Path and Key are the
// only fields the v1 spawner honours.
type CacheEntry struct {
	// Path is the in-container directory to persist. Absolute, or
	// relative to /workspace.
	Path string `yaml:"path"`

	// Key is the cache identity. Same key across runs = volume reused.
	// Supports `{{ checksum "<file>" }}` to mix file contents into the
	// key (cache invalidated when lockfiles change).
	Key string `yaml:"key"`
}
