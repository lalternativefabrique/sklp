package pipeline

// resolve.go pre-computes the parts of a publish.yaml / ci.yaml run that do
// NOT require the LLM: parsing the YAML contract, running `git diff` to find
// the changed paths, applying the service_map and expansion_rules to derive
// impacted_services. Feeding the LLM the resolved set instead of asking it
// to compute one cuts a major source of hallucination (extra services
// inflated, missed services).
//
// The LLM stays in charge of: running preflights, calling the actual build
// commands, scanning the built images, emitting the final JSON report.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/lalternativefabrique/sklp/pipeline/spec"

	"gopkg.in/yaml.v3"
)

// contract mirrors the subset of fields sklp needs to read from the YAML.
// Anything not parsed here is invisible to the resolver but still visible to
// the LLM, which sees the raw YAML in its system prompt.
type contract struct {
	Name           string                     `yaml:"name"`
	ServiceMap     map[string]serviceMapEntry `yaml:"service_map"`
	ExpansionRules []expansionRule            `yaml:"expansion_rules"`
	Inputs         map[string]yaml.Node       `yaml:"inputs"`
	Targets        map[string]targetEntry     `yaml:"targets"`

	// Scopes / Steps are the modern contract shape (all current
	// pipelines use these, not service_map). When service_map is
	// absent, the resolver derives impacted services from the publish
	// steps whose declared scope globs match a changed path.
	Scopes map[string][]string `yaml:"scopes"`
	Steps  []contractStep      `yaml:"steps"`
}

// contractStep mirrors the subset of a `steps:` entry the resolver needs
// to map a publish step to a service and its scope globs. Everything else
// (image, dockerfile, run, …) stays invisible to the resolver.
type contractStep struct {
	Name  string   `yaml:"name"`
	Kind  string   `yaml:"kind"`
	Scope scopeRef `yaml:"scope"`
}

// scopeRef accepts both the scalar (`scope: web`) and list
// (`scope: [web, shared]`) YAML forms, mirroring schema_scope.go.
type scopeRef []string

func (s *scopeRef) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		*s = scopeRef{node.Value}
		return nil
	}
	var list []string
	if err := node.Decode(&list); err != nil {
		return err
	}
	*s = list
	return nil
}

type serviceMapEntry struct {
	ChangedPaths []string `yaml:"changed_paths"`
}

// targetEntry mirrors the `targets.<svc>` block of publish.yaml. Only
// the fields the deterministic resolver needs are decoded here; the rest
// (image template, run command list, notes, …) remain in the raw YAML
// the LLM also sees.
type targetEntry struct {
	// Deps lists service names that MUST be published before this one
	// (for instance: migrate before core, core before web). Used by the
	// topological order computation. Empty for leaf services.
	Deps []string `yaml:"deps"`
}

type expansionRule struct {
	WhenService string   `yaml:"when_service"`
	Publish     []string `yaml:"publish"`
}

// resolvedPlan is the deterministic part of the run, computed before the LLM
// is invoked. It is injected verbatim into the user message so the model
// does not have to (re)derive it.
type resolvedPlan struct {
	Kind             string   // "ci" | "publish"
	ChangedPaths     []string // sorted, deduped
	ImpactedServices []string // sorted, deduped, after expansion

	// PublishOrder is the topological ordering of ImpactedServices
	// derived from targets.<svc>.deps. Services with no declared deps
	// keep their alphabetical position; declared deps push dependents
	// after their dependencies. When the contract declares no deps at
	// all this is just ImpactedServices, alphabetical.
	PublishOrder []string

	BaseRef string
	HeadRef string
}

// resolve reads the YAML contract and the git diff and returns the plan.
// repo is an absolute path. baseRef / headRef default to HEAD~1 / HEAD.
func resolve(yamlBytes []byte, repo, baseRef, headRef string) (*resolvedPlan, error) {
	var c contract
	if err := yaml.Unmarshal(yamlBytes, &c); err != nil {
		return nil, fmt.Errorf("parse contract: %w", err)
	}
	if baseRef == "" {
		baseRef = "HEAD~1"
	}
	if headRef == "" {
		headRef = "HEAD"
	}

	paths, err := gitChangedPaths(repo, baseRef, headRef)
	if err != nil {
		return nil, err
	}

	var impacted []string
	if len(c.ServiceMap) > 0 {
		impacted = impactedServices(c.ServiceMap, c.ExpansionRules, paths)
	} else {
		// Modern contract shape: no service_map, so derive impacted
		// services from the publish steps whose scope globs match a
		// changed path (doublestar, same semantics as the per-step
		// scope gate in pipeline_scope.go).
		impacted, err = impactedStepServices(c.Steps, c.Scopes, paths)
		if err != nil {
			return nil, err
		}
	}
	order := topologicalOrder(impacted, c.Targets)

	return &resolvedPlan{
		Kind:             strings.ToLower(c.Name),
		ChangedPaths:     paths,
		ImpactedServices: impacted,
		PublishOrder:     order,
		BaseRef:          baseRef,
		HeadRef:          headRef,
	}, nil
}

// topologicalOrder sorts impacted services so that every service appears
// AFTER its dependencies (e.g. migrate before core before web). When the
// contract declares no deps for any service, the input order is preserved
// (impacted is already sorted alphabetically by impactedServices).
//
// Cycle handling: services that participate in a cycle are appended at
// the end in alphabetical order — the resolver does not refuse the run,
// it just falls back to a defensible default and lets the LLM see the
// ordering it produced.
func topologicalOrder(impacted []string, targets map[string]targetEntry) []string {
	if len(impacted) <= 1 {
		return append([]string(nil), impacted...)
	}
	inSet := make(map[string]struct{}, len(impacted))
	for _, s := range impacted {
		inSet[s] = struct{}{}
	}
	// Build the adjacency: for each impacted service, deps that are
	// ALSO impacted (deps outside the impacted set are no-ops here —
	// the executor only publishes impacted services).
	indeg := make(map[string]int, len(impacted))
	rdep := make(map[string][]string, len(impacted))
	for _, s := range impacted {
		indeg[s] = 0
	}
	for _, s := range impacted {
		t, ok := targets[s]
		if !ok {
			continue
		}
		for _, d := range t.Deps {
			if _, ok := inSet[d]; !ok {
				continue
			}
			rdep[d] = append(rdep[d], s)
			indeg[s]++
		}
	}
	// Kahn's algorithm, alphabetical tie-break for determinism.
	ready := make([]string, 0, len(impacted))
	for _, s := range impacted {
		if indeg[s] == 0 {
			ready = append(ready, s)
		}
	}
	sort.Strings(ready)
	out := make([]string, 0, len(impacted))
	for len(ready) > 0 {
		s := ready[0]
		ready = ready[1:]
		out = append(out, s)
		next := rdep[s]
		sort.Strings(next)
		for _, nx := range next {
			indeg[nx]--
			if indeg[nx] == 0 {
				ready = append(ready, nx)
			}
		}
		sort.Strings(ready)
	}
	// Append any leftover services (cycle survivors) at the end.
	if len(out) < len(impacted) {
		seen := make(map[string]struct{}, len(out))
		for _, s := range out {
			seen[s] = struct{}{}
		}
		leftover := make([]string, 0)
		for _, s := range impacted {
			if _, ok := seen[s]; !ok {
				leftover = append(leftover, s)
			}
		}
		sort.Strings(leftover)
		out = append(out, leftover...)
	}
	return out
}

// gitChangedPaths returns the sorted, deduped list of paths changed between
// baseRef and headRef in repo. Falls back to an empty slice (not an error)
// when the diff is empty.
func gitChangedPaths(repo, baseRef, headRef string) ([]string, error) {
	cmd := exec.Command("git", "diff", "--name-only", baseRef, headRef)
	cmd.Dir = repo
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git diff %s..%s: %w: %s", baseRef, headRef, err, strings.TrimSpace(stderr.String()))
	}
	seen := map[string]struct{}{}
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		seen[line] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// impactedServices applies the service_map (path prefix match) and
// expansion_rules to derive the set of services that must be acted on.
// A path "impacts" a service if it has any prefix listed under that
// service's changed_paths. If the result contains an expansion source
// (e.g. "shared"), the rule's `publish` list is merged in and the source
// itself is dropped — keeping the set to actually-buildable services.
func impactedServices(svcMap map[string]serviceMapEntry, rules []expansionRule, paths []string) []string {
	hit := map[string]struct{}{}
	for svc, entry := range svcMap {
		for _, prefix := range entry.ChangedPaths {
			for _, p := range paths {
				if pathMatches(p, prefix) {
					hit[svc] = struct{}{}
					break
				}
			}
		}
	}
	for _, rule := range rules {
		if _, ok := hit[rule.WhenService]; ok {
			for _, target := range rule.Publish {
				hit[target] = struct{}{}
			}
			delete(hit, rule.WhenService)
		}
	}
	out := make([]string, 0, len(hit))
	for s := range hit {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// impactedStepServices derives the impacted services from the publish
// steps of a modern contract (no service_map). A `kind: publish` step is
// impacted when at least one of its scope globs matches a changed path,
// reusing the exact doublestar semantics of the per-step scope gate. The
// returned service name is the step name with a trailing "-image" stripped
// (core-image → core) so it lines up with targets/deps naming; a step with
// no scope is treated as always impacted (matching scopeMatches).
func impactedStepServices(steps []contractStep, scopeDefs map[string][]string, paths []string) ([]string, error) {
	hit := map[string]struct{}{}
	for _, st := range steps {
		if st.Kind != "publish" {
			continue
		}
		ok, err := spec.ScopeMatches([]string(st.Scope), scopeDefs, paths)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		hit[serviceName(st.Name)] = struct{}{}
	}
	out := make([]string, 0, len(hit))
	for s := range hit {
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

// serviceName maps a publish step name to its service name by dropping a
// trailing "-image" suffix (the convention across the publish contracts:
// core-image, web-image, migrate-image). Names without the suffix pass
// through unchanged.
func serviceName(stepName string) string {
	return strings.TrimSuffix(stepName, "-image")
}

// pathMatches treats a service_map entry ending with "/" as a directory
// prefix (`apps/core/` matches `apps/core/main.go`), and entries without a
// trailing slash as exact filename matches anchored at the repo root
// (`Makefile` matches the root `Makefile` but not `apps/web/Makefile`).
//
// This mirrors how the existing contracts have been written: directory-like
// entries get `/`, file-like entries don't.
func pathMatches(path, prefix string) bool {
	if strings.HasSuffix(prefix, "/") {
		return strings.HasPrefix(path, prefix)
	}
	return path == prefix
}

// renderForPrompt formats the plan as a short, parseable block the LLM can
// rely on without recomputing anything. Used in the user message.
//
// For `publish` tasks (service_map present), impacted_services is the source
// of truth and the LLM must act on exactly that set. For `ci` tasks (no
// service_map; gating is in each task's `when:` clause), only changed_paths
// is authoritative — the LLM still has to evaluate `any_service` and
// `changed_paths_include` itself.
func (p *resolvedPlan) renderForPrompt() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Pre-resolved plan (computed deterministically, do not recompute):\n")
	fmt.Fprintf(&sb, "  git_diff: %s..%s\n", p.BaseRef, p.HeadRef)
	fmt.Fprintf(&sb, "  changed_paths (%d):\n", len(p.ChangedPaths))
	for _, cp := range p.ChangedPaths {
		fmt.Fprintf(&sb, "    - %s\n", cp)
	}
	if p.Kind == "publish" {
		fmt.Fprintf(&sb, "  impacted_services (%d): %s\n", len(p.ImpactedServices), strings.Join(p.ImpactedServices, ", "))
		if len(p.ImpactedServices) == 0 {
			sb.WriteString("  (no impacted service — emit an empty report and stop)\n")
		} else {
			order := p.PublishOrder
			if len(order) == 0 {
				order = p.ImpactedServices
			}
			fmt.Fprintf(&sb, "  publish_order (topological, dependencies first): %s\n", strings.Join(order, " → "))
			sb.WriteString("  → Act on exactly this set, IN THIS ORDER. Publishing a dependent service before its dependency would deploy a binary against an older schema / API. Do not reorder. Do not add services. Do not skip services unless a preflight aborts_services them.\n")
		}
	} else {
		sb.WriteString("  (CI: evaluate each task's `when:` against changed_paths above; no service_map for CI.)\n")
	}
	return sb.String()
}

// applyForce rewrites the plan as if every service declared in the
// contract's service_map were impacted. Used by the `-force` flag to
// republish corrupted images without dirtying the source tree.
//
// Heuristic to exclude expansion sources (e.g. `shared`): a service whose
// name appears as `when_service` in any expansion_rule is considered a
// "virtual" source that should not itself be built — only its expanded
// targets do. We collapse those into their targets here.
//
// Topological order is recomputed from targets[].deps so the resulting
// publish_order still respects dependencies.
func applyForce(plan *resolvedPlan, yamlBytes []byte) {
	var c contract
	if err := yaml.Unmarshal(yamlBytes, &c); err != nil {
		return
	}

	// Build the set of virtual sources to exclude from the impacted set.
	virtual := map[string]struct{}{}
	for _, rule := range c.ExpansionRules {
		virtual[rule.WhenService] = struct{}{}
	}

	hit := map[string]struct{}{}
	for svc := range c.ServiceMap {
		if _, isVirtual := virtual[svc]; isVirtual {
			continue
		}
		hit[svc] = struct{}{}
	}
	// Modern contract shape (no service_map): every publish step is a
	// buildable service. Mirror the diff-based fallback so -force
	// republishes the full set instead of nothing.
	for _, st := range c.Steps {
		if st.Kind != "publish" {
			continue
		}
		svc := serviceName(st.Name)
		if _, isVirtual := virtual[svc]; isVirtual {
			continue
		}
		hit[svc] = struct{}{}
	}
	// Re-apply expansion rules to materialise the targets of any virtual
	// source. This is a no-op when virtuals are absent from service_map
	// (defensive: keeps the logic identical to the diff-based path).
	for _, rule := range c.ExpansionRules {
		for _, target := range rule.Publish {
			hit[target] = struct{}{}
		}
	}

	impacted := make([]string, 0, len(hit))
	for s := range hit {
		impacted = append(impacted, s)
	}
	sort.Strings(impacted)

	plan.ImpactedServices = impacted
	plan.PublishOrder = topologicalOrder(impacted, c.Targets)
}

// scopeDefsFromTask reads just the `scopes:` block out of a contract, so
// -resolve-only can validate an -only selection without a full pipeline
// load. Returns nil on a malformed file; validation then reports every
// name as unknown, which is the right outcome for an unreadable contract.
func scopeDefsFromTask(yamlBytes []byte) map[string][]string {
	var c contract
	if err := yaml.Unmarshal(yamlBytes, &c); err != nil {
		return nil
	}
	return c.Scopes
}

// applyOnly restricts the plan to the publish steps whose `scope:` names
// one of the selected scopes, mirroring the -only scope matcher used by
// the pipeline compiler. Keeping -resolve-only in sync matters: a plan
// that ignored -only would advertise services the run will never publish.
//
// Unknown scope names are the caller's error to surface (CompilePipeline
// validates them); here they simply select nothing.
func applyOnly(plan *resolvedPlan, yamlBytes []byte, only []string) {
	var c contract
	if err := yaml.Unmarshal(yamlBytes, &c); err != nil {
		return
	}
	want := make(map[string]struct{}, len(only))
	for _, name := range only {
		want[name] = struct{}{}
	}

	hit := map[string]struct{}{}
	for _, st := range c.Steps {
		if st.Kind != "publish" {
			continue
		}
		for _, sc := range st.Scope {
			if _, ok := want[sc]; ok {
				hit[serviceName(st.Name)] = struct{}{}
				break
			}
		}
	}

	impacted := make([]string, 0, len(hit))
	for s := range hit {
		impacted = append(impacted, s)
	}
	sort.Strings(impacted)

	plan.ImpactedServices = impacted
	plan.PublishOrder = topologicalOrder(impacted, c.Targets)
}

// gitFastForward attempts a `git pull --ff-only` against the current
// branch so the impact diff is computed against the actual remote HEAD
// rather than a stale local clone. Best-effort: any failure prints a
// warning on stderr and returns; the caller proceeds with whatever
// local state already exists.
//
// We intentionally avoid plain `git pull` (which can produce a merge
// commit on a diverged branch) and `git fetch && git reset --hard`
// (which throws away uncommitted work). Fast-forward only is the
// safest option that still picks up upstream commits.
//
// A brand-new local branch has no upstream yet — `git pull` would emit
// a scary "There is no tracking information…" multi-line error. That's
// a normal state for a freshly-created feature branch about to be
// pushed for the first time, not a problem to warn about. Detect it
// with rev-parse and skip silently.
func gitFastForward(repoAbs string) {
	check := exec.Command("git", "-C", repoAbs, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	check.Stderr = nil
	if err := check.Run(); err != nil {
		// No upstream configured for the current branch — nothing to
		// fast-forward against. Silent skip; the impact diff still
		// works against HEAD~1 (or whatever -base-ref the caller set).
		return
	}
	cmd := exec.Command("git", "-C", repoAbs, "pull", "--ff-only", "--quiet")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = nil
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		fmt.Fprintf(os.Stderr, "[runner] warning: `git pull --ff-only` failed: %s (continuing with the local state — pass -no-pull to silence this)\n", msg)
	}
}

// envOr returns the value of the environment variable name if set and
// non-empty, otherwise the supplied fallback. Used as the default value of
// a flag.String so operators can configure the runner once via
// SKLP_PROVIDER / SKLP_MODEL / SKLP_BASE_URL instead of repeating the
// flags on every invocation. Explicit CLI flags still override the env.
func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
