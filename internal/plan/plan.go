// Package plan defines the compiled, cache-friendly representation of a
// `.sklp/tasks/*.yaml` contract that the deterministic executor runs without
// invoking an LLM step-by-step.
//
// The flow is "compile once, execute many":
//
//  1. The LLM reads the YAML contract once and emits a Plan{} (this package).
//  2. The Plan is signed and cached on disk (apps/sklp/internal/plan/cache.go).
//  3. Subsequent invocations load the cached Plan and execute it in pure Go.
//  4. The LLM is invoked again only when (a) the cache is invalidated, or
//     (b) a command fails and a diagnosis must be produced.
//
// The Plan schema is intentionally small and explicit: every command is a
// literal string with at most two placeholders (<publish_tag>, <service>)
// that the executor substitutes at run time.
package plan

// SchemaVersion is bumped whenever the Plan struct or any enum below
// changes shape. It is part of the cache key so old plans cannot be
// loaded against a newer executor that would mis-interpret them.
//
// History:
//
//	v1: initial release (PR #182).
//	v2: PreflightStep.Cwd / ServiceSteps.Cwd added so contracts that
//	    write `cd apps/core && govulncheck ./...` compile to a single
//	    command run in the apps/core working dir, instead of a
//	    forbidden shell-composite which bash_allowlisted rejects.
//	v3: KindCI shape added (CISteps + ToolPin). pipeline_exec.go now
//	    compiles `kind: pipeline` YAML into Plan{Kind: KindCI} instead
//	    of executing it directly, so the executor has a single path.
//	v4: CIStep.DependsOn + CIPlan.Adjacency added. Pipeline steps now
//	    form an explicit DAG resolved at compile time (engine 2.2).
//	v5: CIStep gains Cwd / Network / Secrets for the process sandbox
//	    (env allowlist, controlled cwd, materialised secrets, opt-in
//	    network isolation). The sandbox runs in-process; the OCI/K8s
//	    spawners are unaffected.
//	v6: per-CIStep content-addressed cache (engine 2.3). The cache key
//	    derivation hashes (Run, Env, Tools+versions, an inputs-digest
//	    placeholder reserved for 2.4, and the upstream step's key).
//	    Bumping the schema invalidates any v5 plan whose executor did
//	    not know about the cache shape.
//	v7: declared inputs/outputs + content-addressed store for step
//	    artifacts (engine 2.4). CIStep gains Inputs (resolved globs)
//	    and Outputs (literal paths); the step cache key folds the
//	    Merkle digest of the resolved input files, and successful
//	    runs persist declared outputs into .sklp/cache/cas/ for
//	    restoration on a future cache hit.
//	v8: `kind: docker-build` step shape (issue #411). CIStep gains a
//	    StepKind discriminator and an optional Publish spec; the
//	    executor folds build → Trivy scan → push into one cacheable
//	    unit, and the cache key includes the Dockerfile digest, the
//	    resolved context inputs, the image/tag literal, the pinned
//	    Trivy version, and the scan/push flags.
//	v9: BuildKit secrets + build args on docker-build (issue #421).
//	    PublishSpec gains BuildArgs (folded into the cache key by
//	    resolved value) and BuildSecrets (folded by id+source-ref, NOT
//	    value — rotating a secret value must hit cache).
//	v10: CIStep gains Filesystem (Landlock FS confinement policy). The
//	     default empty value confines the step to repo/tmp/secret/system
//	     paths and denies the rest of $HOME; "open" disables it. Folded
//	     into the step cache key so a confined run and an open run never
//	     share an entry.
const SchemaVersion = 10

// Kind enumerates the contract families we know how to compile.
//
// "publish" is the LLM-compiled path for `.sklp/tasks/publish.yaml`
// (build → scan → push per service).
//
// "ci" is the Go-compiled path for `.sklp/tasks/*.yaml` with
// `kind: pipeline`. The runner package compiles `Pipeline{}` directly
// into a Plan with Kind=KindCI; no LLM is involved.
type Kind string

const (
	KindPublish Kind = "publish"
	KindCI      Kind = "ci"
)

// Plan is the compiled contract. The compiler (compile.go) produces it
// from a YAML; the executor (exec.go) consumes it.
//
// All commands are literal: any dynamic value (publish tag, service name)
// appears as a placeholder. The executor substitutes placeholders before
// running, so the cached Plan is independent of any given run.
type Plan struct {
	// SchemaVersion is the version of this struct at compile time. The
	// loader refuses to load a plan whose version does not match the
	// current SchemaVersion constant.
	SchemaVersion int `json:"schema_version"`

	// Kind is the contract family. The executor switches on it to pick
	// the right execution shape (preflights + per-service build/scan/push
	// for publish; per-task run blocks for ci).
	Kind Kind `json:"kind"`

	// PublishTagShell is the shell snippet that produces the publish tag
	// when run at the repo root. It is executed verbatim by the
	// publish_tag preflight; the result is then substituted into
	// <publish_tag> placeholders in service steps.
	//
	// Example (from the current publish.yaml):
	//   printf "%s-%s" "$(git show -s --format=%cd --date=format:%Y%m%d-%H%M HEAD)" "$(git rev-parse --short HEAD)"
	PublishTagShell string `json:"publish_tag_shell,omitempty"`

	// Preflights are the read-only safety checks that run before any
	// build / push. They execute in declared order. A failure with
	// AbortRule.Mode=="all" stops the whole run; Mode=="services" only
	// removes a subset from publish_order.
	Preflights []PreflightStep `json:"preflights"`

	// Services maps service name → its build/scan/push steps. The keys
	// are the same names that appear in ServiceMap and PublishOrder.
	Services map[string]ServiceSteps `json:"services"`

	// ServiceMap maps service name → repo path prefixes that, when
	// changed, mark the service as impacted. Mirrors `service_map` in
	// publish.yaml. The executor combines this with `git diff` to
	// compute impacted_services at run time (no LLM).
	ServiceMap map[string][]string `json:"service_map"`

	// ExpansionRules let a virtual service (e.g. "shared") expand into
	// concrete services to publish. Mirrors `expansion_rules` in
	// publish.yaml.
	ExpansionRules []ExpansionRule `json:"expansion_rules,omitempty"`

	// Deps maps service name → service names that MUST be published
	// before it. Used by the executor's topological sort. Empty for
	// leaf services.
	Deps map[string][]string `json:"deps,omitempty"`

	// CI carries the compiled `kind: pipeline` shape. Non-nil only when
	// Kind == KindCI; the publish path leaves this empty.
	CI *CIPlan `json:"ci,omitempty"`

	// NeedsAgent is set by the compiler when the YAML contains a shape
	// it cannot encode safely as a Plan (conditional commands the
	// executor would mis-interpret, custom interpret blocks, …). When
	// true, the runner falls back to the legacy agent path verbatim
	// and the Plan is NOT cached.
	NeedsAgent bool `json:"needs_agent,omitempty"`

	// NeedsAgentReason is the human-readable reason set alongside
	// NeedsAgent=true. Surfaced to the user via stderr.
	NeedsAgentReason string `json:"needs_agent_reason,omitempty"`
}

// PreflightStep is one read-only safety check. The executor runs
// every Command in order via the bash_allowlisted code path; the
// step is considered "passed" when every command exits zero AND
// the SuccessCheck matches.
type PreflightStep struct {
	// ID is the contract's identifier (e.g. "check-main-branch",
	// "pnpm-audit-prod"). Surfaces in the JSON report as
	// preflight_results[].id — must match the YAML.
	ID string `json:"id"`

	// Description is the short prose from the YAML. Carried for the
	// report and for diagnose context; never executed.
	Description string `json:"description,omitempty"`

	// Cwd is the working directory the commands run in, relative to
	// the repo root. Empty means run at the repo root. This is how
	// contracts that write `cd apps/core && govulncheck ./...`
	// compile down to one command (`govulncheck ./...`) executed in
	// apps/core — bash_allowlisted refuses `cd` and `&&`, so the
	// directory MUST be expressed as metadata, not as a shell verb.
	Cwd string `json:"cwd,omitempty"`

	// Commands are the literal shell commands to run for this
	// preflight, in order. No placeholders — these are read-only
	// safety checks that don't depend on the publish tag.
	Commands []string `json:"commands"`

	// SuccessCheck encodes how to interpret stdout/exit-code beyond
	// "exit code zero". Keeping it as an enum (not free-form code)
	// is what lets the executor run without LLM help.
	SuccessCheck SuccessCheck `json:"success_check"`

	// OnFail tells the executor what to do when SuccessCheck is not
	// met (or the command exits non-zero).
	OnFail AbortRule `json:"on_fail"`
}

// SuccessCheck enumerates the interpretation rules the executor
// supports. Adding a new value here requires bumping SchemaVersion.
//
// The compiler MUST refuse to emit a Plan whose preflight needs a
// check outside this enum (set NeedsAgent=true instead).
type SuccessCheck string

const (
	// SuccessExitZero passes when every command exits 0. Stdout is
	// ignored. Default for most preflights.
	SuccessExitZero SuccessCheck = "exit_zero"

	// SuccessStdoutEmpty passes when every command exits 0 AND the
	// last command's stdout (trimmed) is empty. Used by
	// `git status --short`.
	SuccessStdoutEmpty SuccessCheck = "stdout_empty"

	// SuccessStdoutEquals passes when every command exits 0 AND the
	// last command's stdout (trimmed) equals OnFail.Expected.
	// Used by `git branch --show-current` (expects "main").
	SuccessStdoutEquals SuccessCheck = "stdout_equals"
)

// AbortRule describes the blast radius of a preflight failure. The
// compiler classifies every preflight into one of these modes; the
// executor applies it deterministically.
type AbortRule struct {
	// Mode is how the failure cascades:
	//   - "all":      stop the whole run, skip every service
	//   - "services": skip only the named AbortedServices
	//   - "warn":     record the failure but continue (rare)
	Mode AbortMode `json:"mode"`

	// AbortedServices is the explicit list of services blocked by
	// this preflight when Mode == "services". Ignored otherwise.
	AbortedServices []string `json:"aborted_services,omitempty"`

	// Expected is the stdout string a SuccessStdoutEquals check
	// compares against. Empty for other SuccessChecks.
	Expected string `json:"expected,omitempty"`

	// Message is the user-facing explanation surfaced on failure
	// (in the JSON report's note field).
	Message string `json:"message,omitempty"`
}

// AbortMode is the failure-cascade mode of an AbortRule.
type AbortMode string

const (
	AbortAll      AbortMode = "all"
	AbortServices AbortMode = "services"
	AbortWarn     AbortMode = "warn"
)

// ServiceSteps is the build → scan → push pipeline for a single
// service. The executor runs each list in order; a non-zero exit
// in Build stops the service (no scan, no push); a non-zero exit
// in Scan stops the service (no push).
//
// Every command may contain the placeholders <publish_tag> and
// <service>; the executor substitutes them before running.
type ServiceSteps struct {
	// Image is the registry reference produced by Build, used by
	// Scan and Push. May contain <publish_tag>. Carried for the
	// report (plan_or_published[].image).
	Image string `json:"image"`

	// Cwd is the working directory used for Build/Scan/Push,
	// relative to the repo root. Empty means run at the repo
	// root. Same semantics as PreflightStep.Cwd: it captures a
	// `cd X && ...` from the YAML so the commands themselves stay
	// allowlist-compatible.
	Cwd string `json:"cwd,omitempty"`

	// Build is the list of commands that produce the local image.
	Build []string `json:"build"`

	// Scan is the list of commands that vulnerability-scan the
	// built image. The push only runs if every scan command exits
	// zero. Empty list disables the gate.
	Scan []string `json:"scan,omitempty"`

	// Push is the list of commands that uploads the image to the
	// registry. Runs only if Build and Scan succeeded.
	Push []string `json:"push"`
}

// ExpansionRule lets a "virtual" service (e.g. "shared", which has
// no buildable target of its own) expand into a list of concrete
// services. Mirrors expansion_rules[] in publish.yaml.
type ExpansionRule struct {
	WhenService string   `json:"when_service"`
	Publish     []string `json:"publish"`
}

// ---- KindCI shape (schema v3) -----------------------------------------
//
// A `kind: pipeline` YAML compiles down to a list of CISteps. Everything
// that used to be a runtime conditional in pipeline_exec.go (scope gates,
// env merging, tool selection) is frozen at compile time so the executor
// only has to run a shell line.

// CIPlan groups every CI-specific field. Carried on Plan as a single
// pointer so the publish path pays nothing extra in serialised size.
type CIPlan struct {
	// Steps are the compiled steps in the original YAML order. A step
	// gated out by scope appears here with Status == StatusSkippedCompile
	// and an empty Run; the executor records it as "skipped" without
	// invoking a shell.
	Steps []CIStep `json:"steps"`

	// Tools maps tool name → pinned version. Carried at the plan level
	// for observability; per-step tool subsets live on CIStep.Tools.
	Tools map[string]string `json:"tools,omitempty"`

	// Secrets is the verified-present list of secret names. The compiler
	// fails fast if one is missing, so the executor can assume every
	// name here is already in the inherited environment.
	Secrets []string `json:"secrets,omitempty"`

	// Adjacency is the resolved step DAG: each entry maps a step name
	// to the lex-sorted list of step names that depend on it (i.e. its
	// downstream successors). Carried for the future parallel scheduler
	// (engine 2.5); the sequential executor only iterates Steps in the
	// already topologically-sorted order.
	//
	// A step with no dependents is absent from the map, not an entry
	// with an empty slice, so the JSON encoding stays compact.
	Adjacency map[string][]string `json:"adjacency,omitempty"`
}

// CIStepKind discriminates the compiled step shape. Empty (default) is
// the legacy shell step driven by Run. KindCIStepPublish marks a
// step the executor orchestrates as image build → Trivy scan →
// registry push (issue #411).
type CIStepKind string

const (
	// KindCIStepShell is the implicit default — `sh -c <Run>`.
	KindCIStepShell CIStepKind = ""

	// KindCIStepPublish bundles image build + Trivy scan + registry
	// push into one cacheable step. The CIStep.Publish field
	// carries the resolved config.
	KindCIStepPublish CIStepKind = "publish"
)

// PublishSpec is the resolved configuration for a CIStepKind
// equal to KindCIStepPublish. The compiler validates required
// fields (Image, Tag, Context) and applies defaults (Dockerfile,
// Push, ScanEnabled, ScanSeverity, ScanIgnoreUnfixed).
type PublishSpec struct {
	// Image is the registry reference without the tag, e.g.
	// "rg.fr-par.scw.cloud/waves/sidecar". Combined with Tag to form
	// the full image reference built, scanned, and pushed.
	Image string `json:"image"`

	// Tag is the literal tag (or `$VAR` env interpolation expanded by
	// the executor before building). Empty values are rejected
	// at compile time.
	Tag string `json:"tag"`

	// Context is the build context directory, repo-relative.
	Context string `json:"context"`

	// Dockerfile is the path to the Dockerfile, repo-relative. Always
	// non-empty after compile (defaults to <Context>/Dockerfile).
	Dockerfile string `json:"dockerfile"`

	// Push is the executor's gate on the registry push. Defaults to true.
	Push bool `json:"push"`

	// ScanEnabled gates the Trivy scan phase. Defaults to true.
	ScanEnabled bool `json:"scan_enabled"`

	// ScanSeverity is the comma-joined Trivy --severity filter. Defaults
	// to "HIGH,CRITICAL". Empty when ScanEnabled is false.
	ScanSeverity string `json:"scan_severity,omitempty"`

	// ScanIgnoreUnfixed mirrors Trivy's --ignore-unfixed. Defaults to
	// true.
	ScanIgnoreUnfixed bool `json:"scan_ignore_unfixed"`

	// TrivyImage is the pinned Trivy image reference used to
	// run the scan. Carried on the spec (rather than read from a
	// package constant) so the cache key folds it in via the standard
	// JSON marshalling — bumping the constant invalidates every
	// previously cached publish step.
	TrivyImage string `json:"trivy_image"`

	// BuildArgs overrides Dockerfile ARG values. Values are already
	// template-substituted by the runner.
	// Keys + resolved values enter the cache key (issue #421).
	BuildArgs map[string]string `json:"build_args,omitempty"`

	// Platform is the normalised target os/arch, e.g. "linux/amd64".
	// Always non-empty after compile: an omitted `platform:` resolves
	// to the compiling host's platform.
	Platform string `json:"platform"`

	// BuildSecrets is the BuildKit-style secret list mounted at build time.
	// The cache key folds (id, source kind, source ref) but NOT the
	// resolved value — rotating a secret value must not invalidate the
	// build (issue #421).
	BuildSecrets []PublishSecret `json:"build_secrets,omitempty"`

	// BuildContexts is the named additional-context list (issue #445).
	// Each entry is rendered verbatim — the
	// value may be a relative directory, an absolute path, a
	// `docker-image://...` ref, a git URL, an OCI layout, or a
	// `target:<stage>` reference. BuildKit parses the value.
	//
	// Cache-key semantics: the map enters the spec's JSON marshalling so
	// every (name, value) pair contributes deterministically. Local-dir
	// values additionally cause the runner to append their tree to the
	// step's Inputs (Merkle-style content hash) so editing a file under
	// the directory invalidates the cache.
	BuildContexts map[string]string `json:"build_contexts,omitempty"`
}

// PublishSecret is the resolved BuildKit-secret descriptor for a
// `kind: publish` step. Exactly one of Env / File is non-empty;
// the runner enforces the invariant at compile time.
type PublishSecret struct {
	// ID is the BuildKit secret identifier mounted at
	// /run/secrets/<ID> inside the build sandbox.
	ID string `json:"id"`

	// Env names a runner-process env variable holding the secret value.
	Env string `json:"env,omitempty"`

	// File is a host file path holding the secret value, read at build
	// time. Tilde-expanded by the runner.
	File string `json:"file,omitempty"`
}

// CIStep is one compiled pipeline step.
type CIStep struct {
	// Name is the human label surfaced in the run log.
	Name string `json:"name"`

	// StepKind discriminates the step shape. Empty = shell step (Run).
	// "publish" = wrapper around image build / Trivy / push, with
	// the config in Publish.
	StepKind CIStepKind `json:"step_kind,omitempty"`

	// Publish carries the resolved publish configuration when
	// StepKind == KindCIStepPublish. Nil otherwise.
	Publish *PublishSpec `json:"docker_build,omitempty"`

	// Run is the shell snippet executed via `sh -c`. Empty when the
	// step was gated out at compile (see Status).
	Run string `json:"run,omitempty"`

	// Cwd is the per-step working directory, relative to the repo
	// root. Empty = run at the repo root. The compiler cleans the path
	// and rejects anything escaping the repo root so the sandbox does
	// not have to revalidate at run time.
	Cwd string `json:"cwd,omitempty"`

	// Network is the network isolation policy. "none" asks the
	// in-process sandbox to unshare the network namespace (Linux);
	// any other value is treated as "host network". macOS accepts
	// "none" but logs it as a no-op.
	Network string `json:"network,omitempty"`

	// Filesystem is the filesystem isolation policy. Empty (default)
	// confines the step to repo/tmp/secret/system paths via Landlock
	// on Linux; "open" disables confinement. macOS and Landlock-less
	// kernels run unconfined (logged), never failing for that reason.
	Filesystem string `json:"filesystem,omitempty"`

	// Secrets is the subset of CIPlan.Secrets this step is allowed to
	// see as env vars. Materialisation at /run/secrets/<NAME> is also
	// gated on this list. nil = no secret exposed to the step.
	Secrets []string `json:"secrets,omitempty"`

	// Env is the effective env overlay for this step (pipeline-level
	// Env + step-level Env, with step winning). Empty when no overlay
	// was declared. Resolved secrets do not live here; they are merged
	// from the inherited process env.
	Env map[string]string `json:"env,omitempty"`

	// Tools lists the tool names this step needs, in declared order.
	// The executor passes each name to the host resolver to obtain a
	// bin directory to prepend to PATH. Resolution itself stays at
	// run time so a broken pnpm release does not block compile.
	Tools []string `json:"tools,omitempty"`

	// DependsOn is the resolved predecessor list for this step in the
	// step DAG. The slice is lex-sorted so the JSON encoding is
	// deterministic. For pipelines without any explicit `depends_on`
	// the compiler synthesizes an implicit linear chain (each step
	// depends on the previous one) so the sequential executor and the
	// future parallel scheduler share the same shape.
	DependsOn []string `json:"depends_on,omitempty"`

	// Inputs is the declared list of doublestar globs (engine 2.4).
	// Carried for observability and rebuilds of the resolved set; the
	// authoritative cache-affecting field is ResolvedInputs below.
	Inputs []string `json:"inputs,omitempty"`

	// ResolvedInputs is the sorted list of repo-relative file paths
	// that matched Inputs at compile time. The cache key folds the
	// Merkle digest of these files' content; an unrelated edit (a
	// file not in this list) does NOT invalidate the step.
	ResolvedInputs []string `json:"resolved_inputs,omitempty"`

	// Outputs is the declared list of repo-relative paths (NOT globs)
	// the step produces. After a successful run each existing path is
	// copied into the content-addressed store; on a cache hit the
	// stored content is restored back into the working tree.
	Outputs []string `json:"outputs,omitempty"`

	// Services lists the pipeline services this step references.
	// Carried for the local-services orchestrator (the executor calls
	// a registered hook to boot them before the first dependent step).
	Services []string `json:"services,omitempty"`

	// NoCache is true when the pipeline declared `cache: false` on this
	// step, opting it out of the step cache: it is never looked up and
	// never stored. Set on steps whose effect is external to the repo
	// (a migration resetting a live database, a fixture loader), where
	// a replay would report success without performing the mutation.
	// The compiler resolves the YAML default (cacheable) into this flag,
	// so the executor reads one unambiguous boolean.
	NoCache bool `json:"no_cache,omitempty"`

	// Status is StatusIncluded for steps the executor must run, or
	// StatusSkippedCompile for steps the compiler short-circuited
	// (scope mismatch). The executor refuses any other value.
	Status CIStepStatus `json:"status"`

	// SkipReason explains why Status == StatusSkippedCompile. Surfaced
	// in the report so a user can see why a step did not run.
	SkipReason string `json:"skip_reason,omitempty"`
}

// CIStepStatus is the compile-time status of a CI step.
type CIStepStatus string

const (
	// StatusIncluded means the executor will run this step.
	StatusIncluded CIStepStatus = "included"

	// StatusSkippedCompile means the compiler decided the step should
	// not run (scope gate did not match). The executor records it as
	// "skipped" in the report without spawning a shell.
	StatusSkippedCompile CIStepStatus = "skipped_at_compile"
)
