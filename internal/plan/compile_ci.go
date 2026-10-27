package plan

import (
	"errors"
	"path"
	"strconv"
	"strings"
)

// CompileCIInput is the data the runner package hands to CompileCI.
// It is intentionally framed in plain Go types (no runner.Pipeline)
// so internal/plan does not import the runner package — the dependency
// must stay one-way (runner → internal/plan).
type CompileCIInput struct {
	// Steps are the merged pipeline steps in declared order. Each entry
	// is the resolved step (extends already applied by the caller).
	Steps []CIStepInput

	// PipelineEnv is the pipeline-level env map.
	PipelineEnv map[string]string

	// PipelineTools maps tool name → pinned version.
	PipelineTools map[string]string

	// Secrets is the list of required secret names. The compiler does
	// NOT check presence here (that needs the host environment); the
	// caller (runner.CompilePipeline) handles that fail-fast check.
	Secrets []string

	// ChangedFiles is the pre-computed list of files changed in the
	// current run. Used to evaluate per-step scope gates at compile.
	// Nil / empty means "no information, run every scoped step" — same
	// degraded-open semantics scopeMatches uses today.
	ChangedFiles []string

	// Scopes maps logical scope name → list of doublestar globs.
	Scopes map[string][]string
}

// CIStepInput mirrors runner.Step, but is decoupled from the runner
// types so internal/plan stays import-clean. The runner package
// translates its Step values into this shape before calling CompileCI.
type CIStepInput struct {
	Name       string
	Run        string
	If         string
	Env        map[string]string
	Scope      []string
	Tools      []string // nil = inherit, empty = none, non-empty = subset
	Services   []string
	DependsOn  []string
	Cwd        string
	Network    string
	Filesystem string
	Secrets    []string
	Inputs     []string
	Outputs    []string

	// Cache mirrors the YAML `cache:` opt-out. nil = omitted (the step is
	// cacheable, the default); non-nil false = never cache this step.
	Cache *bool

	// 411: publish — raw fields routed from runner.Step. CompileCI
	// validates them when Kind == "publish" and produces the
	// resolved PublishSpec on the CIStep.
	Kind            string
	Image           string
	Tag             string
	Context         string
	Dockerfile      string
	Push            *bool
	ScanPresent     bool
	ScanDisabled    bool
	ScanSeverity    []string
	ScanIgnoreUnfix *bool

	// 421: BuildKit secrets + build args for `kind: publish`.
	// BuildArgs values are already template-substituted by the caller.
	// BuildSecrets entries are pre-validated (id non-empty, exactly one
	// of Env/File, env presence checked, file tilde-expanded).
	BuildArgs    map[string]string
	BuildSecrets []PublishSecret

	// 445: BuildKit additional contexts (`--build-context name=value`).
	// Values are opaque to sklp — BuildKit parses each ref. Local-dir
	// sources are folded into the cache key by the runner (it appends
	// them to Inputs); image/git refs flow through verbatim.
	BuildContexts map[string]string

	// Platform is the raw `platform:` value of a `kind: publish` step.
	Platform string
}

// ScopeMatcher is the callback CompileCI uses to evaluate a step's
// scope against the changed-file set. The runner package owns the
// doublestar dependency, so we receive the decision as a closure
// rather than re-implementing matching here.
//
// Returning an error stops compilation (an unknown scope name is a
// configuration bug, not a silent skip).
type ScopeMatcher func(stepScopes []string, scopeDefs map[string][]string, changed []string) (bool, error)

// InputResolver expands a step's declared input globs into a sorted
// list of repo-relative file paths. Empty input list → empty result.
// The runner package owns the doublestar dependency; CompileCI receives
// the result as a closure to keep internal/plan import-clean.
//
// Returning an error stops compilation (a malformed glob is a
// configuration bug, not a silent skip).
type InputResolver func(globs []string) ([]string, error)

// CompileCI translates a merged Pipeline into the CI shape of Plan.
// It performs every up-front resolution the issue calls for:
//   - extends:    already merged by the caller (runner.LoadPipeline).
//   - env:        pipeline-level + step-level layered into CIStep.Env.
//   - tools:      pinned versions surfaced on the Plan; per-step subset
//     resolved (nil → inherit pipeline tools, empty → none).
//   - scope / if: gates resolved → step is StatusIncluded or
//     StatusSkippedCompile with a Reason.
//   - secrets:    listed on the Plan; runner verifies presence before
//     invoking CompileCI so the compile step is total.
//
// `if:` is still unsupported (no expression evaluator landed yet);
// any non-empty If value is a compile error so callers fail fast
// instead of silently skipping the step.
func CompileCI(in CompileCIInput, match ScopeMatcher, resolveInputs InputResolver) (*Plan, error) {
	out := &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindCI,
		CI: &CIPlan{
			Steps:   make([]CIStep, 0, len(in.Steps)),
			Tools:   copyStringMap(in.PipelineTools),
			Secrets: append([]string(nil), in.Secrets...),
		},
	}

	// Validate that step names are unique — both for DAG soundness and
	// so error messages can refer to a single owner of a name.
	seen := make(map[string]struct{}, len(in.Steps))
	for _, s := range in.Steps {
		if s.Name == "" {
			return nil, &CICompileError{Reason: "step has no `name`"}
		}
		if _, ok := seen[s.Name]; ok {
			return nil, &CICompileError{Step: s.Name, Reason: "duplicate step name"}
		}
		seen[s.Name] = struct{}{}
	}

	// Resolve dependencies. If no step declares `depends_on`, the
	// compiler synthesizes an implicit linear chain (each step depends
	// on the previous declared step) to preserve historical ordering.
	anyExplicit := false
	for _, s := range in.Steps {
		if len(s.DependsOn) > 0 {
			anyExplicit = true
			break
		}
	}
	resolvedDeps := make(map[string][]string, len(in.Steps))
	nodes := make([]DAGNode, 0, len(in.Steps))
	for i, s := range in.Steps {
		var deps []string
		if anyExplicit {
			deps = append([]string(nil), s.DependsOn...)
		} else if i > 0 {
			deps = []string{in.Steps[i-1].Name}
		}
		sortStrings(deps)
		resolvedDeps[s.Name] = deps
		nodes = append(nodes, DAGNode{Name: s.Name, DependsOn: deps})
	}

	order, err := TopoSort(nodes)
	if err != nil {
		var ce *CycleError
		var me *MissingDepError
		switch {
		case errors.As(err, &ce):
			return nil, &CICompileError{
				Step:   ce.From,
				Reason: "cycle detected between " + ce.From + " and " + ce.To,
			}
		case errors.As(err, &me):
			return nil, &CICompileError{
				Step:   me.From,
				Reason: "depends_on references unknown step " + strconv.Quote(me.Missing),
			}
		default:
			return nil, &CICompileError{Reason: err.Error()}
		}
	}

	// Build the adjacency map (dep -> dependents) once; serialized on
	// the plan so the future parallel scheduler can read it without
	// recomputing.
	adjacency := make(map[string][]string)
	for name, deps := range resolvedDeps {
		for _, d := range deps {
			adjacency[d] = append(adjacency[d], name)
		}
	}
	for k := range adjacency {
		sortStrings(adjacency[k])
	}
	if len(adjacency) > 0 {
		out.CI.Adjacency = adjacency
	}

	// Index inputs by name so we can iterate them in the topo order.
	byName := make(map[string]CIStepInput, len(in.Steps))
	for _, s := range in.Steps {
		byName[s.Name] = s
	}

	for _, name := range order {
		s := byName[name]
		cs := CIStep{
			Name:      s.Name,
			Services:  append([]string(nil), s.Services...),
			DependsOn: resolvedDeps[s.Name],
			// Resolve the YAML default here so the executor never has to
			// reason about an omitted value: absent = cacheable.
			NoCache: s.Cache != nil && !*s.Cache,
		}

		// `if:` is NOT supported. It's rejected at compile (not runtime)
		// so the Plan never carries unresolved branching, and the message
		// points at the working alternative: a shell conditional inside
		// `run:`. Conditional step EXECUTION (skip the whole step) is not
		// a feature; gate behaviour from within the command instead, e.g.
		//   run: |
		//     if [ -z "$STRIPE_SECRET_KEY" ]; then echo "skip: no key"; exit 0; fi
		//     pnpm build
		if s.If != "" {
			return nil, &CICompileError{
				Step:   s.Name,
				Reason: "the `if:` step field is not supported — put the condition in `run:` as a shell test instead (e.g. `if [ -z \"$VAR\" ]; then exit 0; fi; <cmd>`)",
			}
		}

		// Scope gate.
		if len(s.Scope) > 0 {
			ok, err := match(s.Scope, in.Scopes, in.ChangedFiles)
			if err != nil {
				return nil, &CICompileError{Step: s.Name, Reason: err.Error()}
			}
			if !ok {
				cs.Status = StatusSkippedCompile
				cs.SkipReason = "no changes in scope"
				out.CI.Steps = append(out.CI.Steps, cs)
				continue
			}
		}

		var dbSpec *PublishSpec
		if s.Kind == string(KindCIStepPublish) {
			spec, err := compilePublish(s)
			if err != nil {
				return nil, &CICompileError{Step: s.Name, Reason: err.Error()}
			}
			dbSpec = spec
		} else if s.Kind != "" {
			return nil, &CICompileError{
				Step:   s.Name,
				Reason: "unknown step kind " + strconv.Quote(s.Kind),
			}
		}

		// `run:` is mandatory for plain shell steps; publish steps
		// provide their own execution shape.
		if dbSpec == nil && s.Run == "" {
			return nil, &CICompileError{
				Step:   s.Name,
				Reason: "step has no `run:` command",
			}
		}
		if dbSpec != nil && s.Run != "" {
			return nil, &CICompileError{
				Step:   s.Name,
				Reason: "kind: publish step must not declare `run:`",
			}
		}

		cwd, err := cleanStepCwd(s.Cwd)
		if err != nil {
			return nil, &CICompileError{Step: s.Name, Reason: err.Error()}
		}

		if err := validateStepSecrets(s.Secrets, in.Secrets); err != nil {
			return nil, &CICompileError{Step: s.Name, Reason: err.Error()}
		}

		if err := validateRelPaths(s.Inputs, "inputs"); err != nil {
			return nil, &CICompileError{Step: s.Name, Reason: err.Error()}
		}
		if err := validateRelPaths(s.Outputs, "outputs"); err != nil {
			return nil, &CICompileError{Step: s.Name, Reason: err.Error()}
		}

		var resolved []string
		if len(s.Inputs) > 0 && resolveInputs != nil {
			r, err := resolveInputs(s.Inputs)
			if err != nil {
				return nil, &CICompileError{Step: s.Name, Reason: "resolve inputs: " + err.Error()}
			}
			resolved = r
		}

		cs.Status = StatusIncluded
		cs.Run = s.Run
		cs.Cwd = cwd
		cs.Network = s.Network
		cs.Filesystem = s.Filesystem
		cs.Secrets = append([]string(nil), s.Secrets...)
		cs.Env = mergeStringMaps(in.PipelineEnv, s.Env)
		cs.Tools = resolveStepTools(s.Tools, in.PipelineTools)
		cs.Inputs = append([]string(nil), s.Inputs...)
		cs.ResolvedInputs = resolved
		cs.Outputs = append([]string(nil), s.Outputs...)
		if dbSpec != nil {
			cs.StepKind = KindCIStepPublish
			cs.Publish = dbSpec
		}
		out.CI.Steps = append(out.CI.Steps, cs)
	}

	return out, nil
}

// validateRelPaths rejects any path that is absolute, empty, or
// escapes the repo root via "..". Used for declared inputs/outputs
// so the runtime never has to revalidate path safety.
func validateRelPaths(paths []string, field string) error {
	for _, p := range paths {
		if p == "" {
			return &pathEscapeError{Raw: p, Field: field, Reason: "is empty"}
		}
		if strings.HasPrefix(p, "/") {
			return &pathEscapeError{Raw: p, Field: field, Reason: "must be relative to the repo root"}
		}
		clean := path.Clean(p)
		if clean == ".." || strings.HasPrefix(clean, "../") {
			return &pathEscapeError{Raw: p, Field: field, Reason: "escapes the repo root"}
		}
	}
	return nil
}

type pathEscapeError struct {
	Raw    string
	Field  string
	Reason string
}

func (e *pathEscapeError) Error() string {
	return e.Field + " " + strconv.Quote(e.Raw) + " " + e.Reason
}

// cleanStepCwd normalises a step's cwd: empty stays empty, otherwise the
// path is cleaned and rejected if it is absolute or escapes the repo
// root via "..". The runner translates this into a step-level failure.
func cleanStepCwd(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if strings.HasPrefix(raw, "/") {
		return "", &cwdEscapeError{Raw: raw, Reason: "must be relative to the repo root"}
	}
	clean := path.Clean(raw)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", &cwdEscapeError{Raw: raw, Reason: "escapes the repo root"}
	}
	if clean == "." {
		return "", nil
	}
	return clean, nil
}

// cwdEscapeError is returned by cleanStepCwd. It is wrapped inside a
// CICompileError by the caller; carrying it as a typed value lets the
// sandbox tests assert on the rejection without string matching.
type cwdEscapeError struct {
	Raw    string
	Reason string
}

func (e *cwdEscapeError) Error() string {
	return "cwd " + e.Raw + " " + e.Reason
}

// validateStepSecrets ensures every name a step references is actually
// declared at the pipeline level. Mirrors the tools validation: an
// unknown secret name is a configuration bug, not a silent skip.
func validateStepSecrets(stepSecrets, pipelineSecrets []string) error {
	if len(stepSecrets) == 0 {
		return nil
	}
	allowed := make(map[string]struct{}, len(pipelineSecrets))
	for _, s := range pipelineSecrets {
		allowed[s] = struct{}{}
	}
	for _, s := range stepSecrets {
		if _, ok := allowed[s]; !ok {
			return &unknownSecretError{Name: s}
		}
	}
	return nil
}

type unknownSecretError struct{ Name string }

func (e *unknownSecretError) Error() string {
	return "secret " + e.Name + " is not declared at the pipeline level"
}

// CICompileError is returned for any deterministic compile failure.
// Carries the step name and a short reason so the caller can render
// "step X: <reason>" without re-parsing the message.
type CICompileError struct {
	Step   string
	Reason string
	// Cause, when non-nil, is the typed underlying error. Surfaced via
	// Unwrap so the CLI compile-failure printer can find an attached
	// typed error (e.g. BuildKitSecretEnvMissingError) and render a
	// precise remediation hint without re-parsing Reason.
	Cause error
}

func (e *CICompileError) Error() string {
	if e.Step == "" {
		return "ci compile: " + e.Reason
	}
	return "ci compile: step " + e.Step + ": " + e.Reason
}

func (e *CICompileError) Unwrap() error { return e.Cause }

// resolveStepTools applies the three-state contract documented on
// runner.stepTools:
//   - nil:   inherit every pipeline tool (legacy default).
//   - []:    explicit opt-out.
//   - [...]: keep as-is (validation lives in the runner caller).
//
// The returned slice is independent of the inputs so later mutations
// of pipelineTools do not leak into the plan.
func resolveStepTools(stepTools []string, pipelineTools map[string]string) []string {
	if stepTools == nil {
		out := make([]string, 0, len(pipelineTools))
		for name := range pipelineTools {
			out = append(out, name)
		}
		sortStrings(out)
		return out
	}
	if len(stepTools) == 0 {
		return nil
	}
	return append([]string(nil), stepTools...)
}

// mergeStringMaps overlays b onto a (b wins). Returns nil when both
// are empty so the JSON encoding stays compact.
func mergeStringMaps(a, b map[string]string) map[string]string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func copyStringMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// sortStrings is a tiny in-place sort that avoids pulling sort into
// this file's import block; the slices it sorts are tool-name lists
// of at most a handful of entries.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
