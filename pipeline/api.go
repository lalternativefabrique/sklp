// Package runner — programmatic API.
//
// Run(ctx, cfg) is the single entry point used by both the sklp CLI and
// the core/ci HTTP service. It executes a .sklp/tasks/*.yaml contract
// against a repository and returns a structured Report — without
// touching os.Args, os.Exit, or the global loggers.
//
// The CLI (cmd/sklp) and the legacy sklp CLI keep their
// flag-parsing entry points (Main), but those are now thin shells over
// Run.
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// RunConfig is the library-friendly input to Run. Every field maps to
// the equivalent CLI flag; callers that want flag-defaulting from env
// vars (SKLP_PROVIDER, SKLP_MODEL, SKLP_BASE_URL) should resolve those
// themselves before calling.
type RunConfig struct {
	TaskPath string // path to .sklp/tasks/*.yaml (absolute or relative to RepoPath)
	RepoPath string // path to the target repository root (must exist)
	Provider string // "ovh" | "scaleway" | "mistral" | "deepseek" | "anthropic" | "openai" | "openai-compat"
	Model    string // provider-specific model id
	BaseURL  string // required for openai-compat / ovh / scaleway
	APIKey   string // resolved by the caller (env, vault, …)
	DryRun   bool
	MaxSteps int    // retained for API compatibility; unused (no LLM path)
	BaseRef  string // git ref for the impact diff base; defaults to HEAD~1
	Force    bool
	// Only restricts the run to the steps naming one of these scopes,
	// bypassing the diff for them. See CompilePipelineOptions.Only.
	Only    []string
	NoPull  bool
	NoCache bool // bypass the per-step content-addressed cache (engine 2.3)
	Resume  bool // skip steps that passed/were cached in the previous run; restart at the first failure (engine 2.7)

	// NoRemoteCache, when true, disables both lookup and push to the
	// configured remote cache for this run. Local cache still applies
	// unless NoCache is also set.
	NoRemoteCache bool

	// Jobs caps the number of CI steps that may run concurrently.
	// 0 (the default) means the run's CPU budget (SKLP_CPUS, cgroup
	// cpu.max, else runtime.NumCPU()); 1 reproduces the pre-2.5
	// sequential behaviour (engine 2.5).
	Jobs int

	// FailFast cancels every in-flight step on the first failure
	// instead of letting independent siblings finish (engine 2.5).
	FailFast bool

	// Quiet, when true, suppresses live stdout/stderr streaming under
	// the spinner for `kind: pipeline` runs (engine 2.9). Restores the
	// pre-2.9 silent-spinner UX.
	Quiet bool

	// TailLines overrides the per-step rolling-tail size in the live
	// reporter. 0 = auto (min(10, terminal_height/4), floor 3). #444.
	TailLines int

	// EnableHotkeys arms the interactive +/-/f/q listener in the live
	// reporter. Defaults to true on TTY stdin; the runner clears this
	// when -no-hotkeys is passed or when stdin is not a terminal. #444.
	EnableHotkeys bool

	// Stdout / Stderr are the sinks for human-readable progress and
	// the final JSON report. When nil, os.Stdout / os.Stderr are used.
	// core/ci injects io.Writers that pipe to NATS for SSE streaming;
	// the CLI injects the real os streams.
	Stdout io.Writer
	Stderr io.Writer

	// JSONOutput, when true, makes Run emit ONLY the final JSON report
	// on Stdout. All other lines (progress, errors) go to Stderr. This
	// matches the CLI's `-output json` flag and is what core/ci relies
	// on to capture a clean report.
	JSONOutput bool
}

// Report is the structured result of a run. RawJSON is the final
// payload the agent printed; the typed fields are best-effort decoded
// from it for callers that don't want to re-parse.
type Report struct {
	RawJSON string // the JSON object the agent emitted, or "" on early failure
	Phase   string // "ci" | "dry-run" | "apply"
	Kind    string // "ci" | "publish"
	// PassFail is set for ci reports ("pass" | "fail"). Empty for publish.
	PassFail string
	// ImpactedServices, when known, lists the services touched by the run.
	ImpactedServices []string
}

// ErrInvalidConfig is returned when RunConfig is missing required fields.
var ErrInvalidConfig = errors.New("invalid runner config")

// Run executes the contract described by cfg and returns the report.
// Errors returned cover invalid config, missing inputs, provider /
// agent errors, and non-zero exit codes from the agent's final report.
// Report is populated even on error when a partial JSON is available.
func Run(ctx context.Context, cfg RunConfig) (Report, error) {
	if cfg.TaskPath == "" || cfg.RepoPath == "" {
		return Report{}, fmt.Errorf("%w: TaskPath and RepoPath are required", ErrInvalidConfig)
	}
	stdout := cfg.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := cfg.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	repoAbs, err := filepath.Abs(cfg.RepoPath)
	if err != nil {
		return Report{}, fmt.Errorf("repo abs: %w", err)
	}

	if err := WireGitHubToken(stderr); err != nil {
		fmt.Fprintf(stderr, "[sklp] warning: wire GITHUB_TOKEN: %v\n", err)
	}
	_ = MarkWorkspaceSafe(repoAbs, stderr)

	if !cfg.NoPull {
		gitFastForward(repoAbs)
	}

	taskAbs := cfg.TaskPath
	if !filepath.IsAbs(taskAbs) {
		taskAbs = filepath.Join(repoAbs, taskAbs)
	}
	taskAbs, err = filepath.Abs(taskAbs)
	if err != nil {
		return Report{}, fmt.Errorf("task abs: %w", err)
	}
	taskBytes, err := os.ReadFile(taskAbs)
	if err != nil {
		return Report{}, fmt.Errorf("read task %s: %w", taskAbs, err)
	}

	// New schema fast path: if the YAML declares `kind: pipeline`, run
	// the deterministic step-by-step executor. No LLM, no plan cache,
	// no agent — just a shell loop over Steps with Env / Secrets /
	// Tools honored.
	if isPipelineKind(taskBytes) {
		return runPipelineFromBytes(ctx, cfg, taskAbs, repoAbs, stdout, stderr)
	}

	effectiveBase := cfg.BaseRef
	if effectiveBase == "" {
		effectiveBase = "HEAD~1"
	}
	plan, err := resolve(taskBytes, repoAbs, effectiveBase, "HEAD")
	if err != nil {
		return Report{}, fmt.Errorf("resolve plan: %w", err)
	}
	if cfg.Force {
		applyForce(plan, taskBytes)
	}
	kind := plan.Kind
	if kind == "" {
		kind = detectKind(string(taskBytes))
	}

	// `kind: ci` no longer has an executor: CI runs exclusively through
	// `kind: pipeline` (routed earlier). The legacy LLM-agent CI path has
	// been removed. Reject with an actionable error.
	if kind == "ci" {
		return Report{}, fmt.Errorf(
			"%w: %s declares the legacy CI contract (kind: ci); its executor has been removed — convert it to kind: pipeline with a steps: block",
			ErrInvalidConfig, taskAbs,
		)
	}

	// `kind: publish` was driven by the LLM agent / plan-cache compile,
	// both of which have been removed. Reject with the same actionable
	// guidance: publish must be expressed as a kind: pipeline contract.
	if kind == "publish" {
		return Report{}, fmt.Errorf(
			"%w: %s declares the legacy publish contract (kind: publish); its executor has been removed — convert it to kind: pipeline with a steps: block",
			ErrInvalidConfig, taskAbs,
		)
	}

	return Report{}, fmt.Errorf("unsupported task kind %q (expected pipeline)", kind)
}

// decodeReport produces a typed Report from the agent's raw JSON.
// Safe on empty / malformed input.
func decodeReport(raw, kind string, dryRun bool, plan *resolvedPlan) Report {
	rep := Report{RawJSON: raw, Kind: kind}
	if kind == "publish" {
		if dryRun {
			rep.Phase = "dry-run"
		} else {
			rep.Phase = "apply"
		}
	} else {
		rep.Phase = kind
	}
	if plan != nil {
		rep.ImpactedServices = append([]string{}, plan.ImpactedServices...)
	}
	if raw == "" {
		return rep
	}
	var probe struct {
		Phase            string   `json:"phase"`
		PassFail         string   `json:"pass_fail"`
		ImpactedServices []string `json:"impacted_services"`
	}
	if err := json.Unmarshal([]byte(raw), &probe); err == nil {
		if probe.Phase != "" {
			rep.Phase = probe.Phase
		}
		rep.PassFail = probe.PassFail
		if len(probe.ImpactedServices) > 0 {
			rep.ImpactedServices = probe.ImpactedServices
		}
	}
	return rep
}
