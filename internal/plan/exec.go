package plan

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Report is the deterministic executor's output. Mirrors the JSON
// shape today's agent runner emits in its final message, so callers
// (skalpel UI, CI consumers, `sklp publish` stdout) see no difference
// between agent and plan-executed runs.
//
// Fields not surfaced by the agent path (e.g. RuntimeMs) are tagged
// omitempty so existing consumers keep parsing the same JSON.
type Report struct {
	Phase            string            `json:"phase"`
	ImpactedServices []string          `json:"impacted_services"`
	PublishOrder     []string          `json:"publish_order"`
	PreflightResults []PreflightResult `json:"preflight_results"`
	PlanOrPublished  []ServiceResult   `json:"plan_or_published"`
	RuntimeMs        int64             `json:"runtime_ms,omitempty"`
}

// PreflightResult is one entry of Report.PreflightResults.
type PreflightResult struct {
	ID             string     `json:"id"`
	Status         StepStatus `json:"status"`
	Note           string     `json:"note,omitempty"`
	AbortsServices []string   `json:"aborts_services"`
	Diagnosis      *Diagnosis `json:"diagnosis,omitempty"`
}

// ServiceResult is one entry of Report.PlanOrPublished.
type ServiceResult struct {
	Service   string     `json:"service"`
	Image     string     `json:"image"`
	Command   string     `json:"command,omitempty"`
	Status    StepStatus `json:"status"`
	BlockedBy string     `json:"blocked_by,omitempty"`
	Note      string     `json:"note,omitempty"`
	Diagnosis *Diagnosis `json:"diagnosis,omitempty"`
}

// StepStatus is the per-step status code shared by preflights and
// services. Matches the values today's agent prompt requires.
type StepStatus string

const (
	StatusPassed    StepStatus = "passed"
	StatusFailed    StepStatus = "failed"
	StatusSkipped   StepStatus = "skipped"
	StatusSucceeded StepStatus = "succeeded"
	StatusPlanned   StepStatus = "planned" // dry-run
	StatusCached    StepStatus = "cached"  // CI step served from content-addressed cache (engine 2.3)
)

// Diagnosis carries an optional fix proposal attached to a failure.
// Populated by the diagnose-on-failure step (T7); empty otherwise.
type Diagnosis struct {
	RootCause         string `json:"root_cause"`
	LikelyExplanation string `json:"likely_explanation"`
	SuggestedFix      string `json:"suggested_fix"`
}

// ExecConfig configures execPlan. The DryRun flag mirrors today's
// `sklp publish -dry-run`: preflights still run (they are read-only),
// but build/scan/push are recorded as "planned" without execution.
type ExecConfig struct {
	// Plan is the Plan to execute. Must have already passed Validate.
	Plan *Plan
	// Repo is the absolute repo root. Commands run with Dir set here.
	Repo string
	// ImpactedServices is the set of services the executor should act
	// on, in topological order. Pre-computed by resolve.go from the
	// git diff so it doesn't depend on the LLM.
	ImpactedServices []string
	// PublishTag is the substituted value of <publish_tag>. The
	// executor pre-computes it by running Plan.PublishTagShell.
	// Provided here so callers (and tests) can inject a deterministic
	// tag instead of forking `git`.
	PublishTag string
	// DryRun, when true, skips state-changing service steps. Preflights
	// still execute since they are read-only.
	DryRun bool
	// CmdTimeout caps each individual command. 5 minutes mirrors the
	// agent runner's per-tool timeout for parity.
	CmdTimeout time.Duration
	// Allowlist mirrors tools.BashAllowlistedConfig.Allowlist — entries
	// are bare basenames ("git", "make") or program+sub ("git diff").
	// Validate has already checked the plan against this list; we
	// re-check at exec time as defense in depth.
	Allowlist []string
	// OnCommandLine, if set, is invoked for every stdout/stderr line
	// of every executed command. Used by the runner to mirror today's
	// "│ make: ..." live trace.
	OnCommandLine func(stream, command, line string)
	// OnStepStart fires before a preflight or service phase begins.
	// Lets the caller print "▶ preflight check-main-branch…" so the
	// operator sees what is happening in real time instead of
	// staring at a silent terminal for minutes. Safe to leave nil.
	OnStepStart func(ev StepEvent)
	// OnStepEnd fires after a preflight or service phase finishes,
	// carrying status + duration. The runner uses it to print the
	// "✓ 12.4s" line and to colour failures.
	OnStepEnd func(ev StepEvent)
	// Diagnose, if set, is called for every failed step (preflight or
	// service) with a short failure context. The returned Diagnosis is
	// attached to the corresponding report entry. Keeping it as a
	// callback (instead of importing the diagnose package directly)
	// avoids a circular import and lets tests inject a fake diagnoser.
	//
	// The function must be cheap to invoke: the executor calls it
	// synchronously, so a slow LLM call delays the final report. Wire
	// it with a short overall timeout (≤ 60s).
	Diagnose DiagnoseFn
}

// DiagnoseFn is the callback shape Execute uses to produce a
// Diagnosis for a failed step. ctx is the executor's outer context;
// failure carries everything the diagnoser needs to ground its answer
// in the actual stderr / stdout / exit code observed.
type DiagnoseFn func(ctx context.Context, failure FailureContext) *Diagnosis

// StepEvent describes a preflight or service phase transition.
// Emitted to OnStepStart (before) and OnStepEnd (after) so the
// runner can render live progress without polling the Report.
//
// Duration is meaningful only on OnStepEnd; zero on OnStepStart.
type StepEvent struct {
	// Kind is "preflight" or "service".
	Kind string
	// StepID identifies which preflight or service is involved.
	StepID string
	// Phase is the service sub-phase ("build", "scan", "push").
	// Empty for preflights.
	Phase string
	// Status is the final status on OnStepEnd: passed / succeeded /
	// failed / skipped / planned. Empty on OnStepStart.
	Status StepStatus
	// Duration is the wall-clock time spent on this step.
	// Set only on OnStepEnd.
	Duration time.Duration
	// Note carries the short failure / skip reason on OnStepEnd
	// when Status is failed or skipped.
	Note string
}

// FailureContext is what the executor hands to a DiagnoseFn. Kept
// small on purpose: a good diagnosis needs the failing command and
// the stderr tail, not the whole repo.
type FailureContext struct {
	// Kind is "preflight" or "service".
	Kind string
	// StepID identifies which preflight (e.g. "pnpm-audit-prod") or
	// which service (e.g. "web") failed. Used by the diagnoser's
	// prompt to anchor the answer.
	StepID string
	// Phase is the sub-phase that failed for a service: "build",
	// "scan", or "push". Empty for preflights.
	Phase string
	// Command is the literal substituted shell line that exited
	// non-zero. Carries the resolved <publish_tag> / <service>.
	Command string
	// ExitCode is the OS exit status of the failing command. Zero
	// when the failure came from a SuccessCheck mismatch rather than
	// from the program itself.
	ExitCode int
	// Stdout / Stderr are the full output of the failing command,
	// not yet truncated. The diagnoser decides how much to feed the
	// LLM (typically 8 KB of stderr tail).
	Stdout string
	// Stderr captures the failing command's stderr.
	Stderr string
}

// Execute runs the Plan in cfg.Repo and returns a Report. It is the
// LLM-free counterpart to today's agent loop: preflights in order,
// then build → scan → push per service in PublishOrder.
//
// The function is deterministic: identical inputs (plan, repo state,
// publish tag, dry-run flag) produce identical reports. The only
// non-determinism is the duration of each command, captured separately
// in Report.RuntimeMs.
func Execute(ctx context.Context, cfg ExecConfig) (*Report, error) {
	if cfg.Plan == nil {
		return nil, errors.New("Execute: plan is nil")
	}
	if cfg.Repo == "" {
		return nil, errors.New("Execute: repo path is empty")
	}
	if cfg.CmdTimeout == 0 {
		cfg.CmdTimeout = 5 * time.Minute
	}

	phase := "apply"
	if cfg.DryRun {
		phase = "dry-run"
	}

	allow := compileAllowlist(cfg.Allowlist)
	rep := &Report{
		Phase:            phase,
		ImpactedServices: append([]string(nil), cfg.ImpactedServices...),
		PublishOrder:     append([]string(nil), cfg.ImpactedServices...),
		PreflightResults: make([]PreflightResult, 0, len(cfg.Plan.Preflights)),
		PlanOrPublished:  make([]ServiceResult, 0, len(cfg.ImpactedServices)),
	}
	sort.Strings(rep.ImpactedServices) // unordered set, sorted for stability

	t0 := time.Now()
	defer func() { rep.RuntimeMs = time.Since(t0).Milliseconds() }()

	// --- Preflights -------------------------------------------------
	//
	// Run every preflight. Collect aborts incrementally — a preflight
	// with Mode=AbortAll short-circuits the whole run; one with
	// Mode=AbortServices removes services from publish_order.
	aborted := make(map[string]string) // service → preflight id that blocked it
	for _, pf := range cfg.Plan.Preflights {
		if cfg.OnStepStart != nil {
			cfg.OnStepStart(StepEvent{Kind: "preflight", StepID: pf.ID})
		}
		pfStart := time.Now()
		res, failureCtx := runPreflight(ctx, pf, cfg, allow)
		if res.Status == StatusFailed && cfg.Diagnose != nil && failureCtx != nil {
			res.Diagnosis = cfg.Diagnose(ctx, *failureCtx)
		}
		if cfg.OnStepEnd != nil {
			cfg.OnStepEnd(StepEvent{
				Kind: "preflight", StepID: pf.ID,
				Status: res.Status, Duration: time.Since(pfStart), Note: res.Note,
			})
		}
		rep.PreflightResults = append(rep.PreflightResults, res)
		if res.Status != StatusFailed {
			continue
		}
		switch pf.OnFail.Mode {
		case AbortAll:
			// Whole run stops. Mark every impacted service as skipped.
			for _, svc := range cfg.ImpactedServices {
				rep.PlanOrPublished = append(rep.PlanOrPublished, ServiceResult{
					Service:   svc,
					Image:     substitute(cfg.Plan.Services[svc].Image, svc, cfg.PublishTag),
					Status:    StatusSkipped,
					BlockedBy: pf.ID,
					Note:      "blocked by preflight " + pf.ID,
				})
			}
			return rep, nil
		case AbortServices:
			for _, svc := range pf.OnFail.AbortedServices {
				if _, already := aborted[svc]; !already {
					aborted[svc] = pf.ID
				}
			}
		case AbortWarn:
			// Continue — the failure is recorded for visibility but
			// does not block any service.
		}
	}

	// --- Services: build → scan → push -----------------------------
	for _, svc := range cfg.ImpactedServices {
		steps, ok := cfg.Plan.Services[svc]
		if !ok {
			rep.PlanOrPublished = append(rep.PlanOrPublished, ServiceResult{
				Service: svc,
				Status:  StatusFailed,
				Note:    "service not declared in plan.services",
			})
			continue
		}
		image := substitute(steps.Image, svc, cfg.PublishTag)

		if pf, blocked := aborted[svc]; blocked {
			rep.PlanOrPublished = append(rep.PlanOrPublished, ServiceResult{
				Service:   svc,
				Image:     image,
				Status:    StatusSkipped,
				BlockedBy: pf,
				Note:      "blocked by preflight " + pf,
			})
			continue
		}

		if cfg.DryRun {
			rep.PlanOrPublished = append(rep.PlanOrPublished, ServiceResult{
				Service: svc,
				Image:   image,
				Command: substitute(joinCommands(steps.Build, steps.Scan, steps.Push), svc, cfg.PublishTag),
				Status:  StatusPlanned,
			})
			continue
		}

		// Apply mode: build, scan, push — stop on first non-zero.
		entry := ServiceResult{Service: svc, Image: image}
		ranAll := true
		var failure *FailureContext
		for _, phase := range []struct {
			name string
			cmds []string
		}{
			{"build", steps.Build},
			{"scan", steps.Scan},
			{"push", steps.Push},
		} {
			if !ranAll {
				break
			}
			if len(phase.cmds) == 0 {
				continue
			}
			if cfg.OnStepStart != nil {
				cfg.OnStepStart(StepEvent{Kind: "service", StepID: svc, Phase: phase.name})
			}
			phaseStart := time.Now()
			phaseStatus := StatusSucceeded
			var phaseNote string
			for _, raw := range phase.cmds {
				cmd := substitute(raw, svc, cfg.PublishTag)
				code, stdout, stderr, err := runShellCommand(ctx, cmd, cfg.Repo, steps.Cwd, allow, cfg.CmdTimeout, cfg.OnCommandLine)
				if err != nil || code != 0 {
					ranAll = false
					exitCode := code
					stderrText := stderr
					if err != nil {
						exitCode = -1
						if stderrText == "" {
							stderrText = err.Error()
						}
					}
					failure = &FailureContext{
						Kind: "service", StepID: svc, Phase: phase.name,
						Command: cmd, ExitCode: exitCode,
						Stdout: stdout, Stderr: stderrText,
					}
					phaseStatus = StatusFailed
					phaseNote = truncateLine(stderrText, 200)
					break
				}
			}
			if cfg.OnStepEnd != nil {
				cfg.OnStepEnd(StepEvent{
					Kind: "service", StepID: svc, Phase: phase.name,
					Status: phaseStatus, Duration: time.Since(phaseStart), Note: phaseNote,
				})
			}
		}
		if !ranAll {
			entry.Status = StatusFailed
			entry.Note = fmt.Sprintf("%s failed: %s", failure.Phase, truncateLine(failure.Stderr, 400))
			if cfg.Diagnose != nil {
				entry.Diagnosis = cfg.Diagnose(ctx, *failure)
			}
		} else {
			entry.Status = StatusSucceeded
		}
		rep.PlanOrPublished = append(rep.PlanOrPublished, entry)

		// Hard stop on first failed service: dependent services later
		// in the publish_order would be deploying against a missing /
		// older artifact. The agent runner enforces the same rule.
		if !ranAll {
			for _, later := range remainingAfter(cfg.ImpactedServices, svc) {
				lat, ok2 := cfg.Plan.Services[later]
				img := ""
				if ok2 {
					img = substitute(lat.Image, later, cfg.PublishTag)
				}
				rep.PlanOrPublished = append(rep.PlanOrPublished, ServiceResult{
					Service:   later,
					Image:     img,
					Status:    StatusSkipped,
					BlockedBy: svc,
					Note:      fmt.Sprintf("upstream service %q failed", svc),
				})
			}
			return rep, nil
		}
	}

	return rep, nil
}

// runPreflight runs every command of one preflight in order, applies
// the SuccessCheck, and returns a PreflightResult plus the
// FailureContext to feed a diagnoser (nil when the preflight passed).
func runPreflight(ctx context.Context, pf PreflightStep, cfg ExecConfig, allow allowSets) (PreflightResult, *FailureContext) {
	res := PreflightResult{ID: pf.ID, AbortsServices: []string{}}
	if len(pf.Commands) == 0 {
		res.Status = StatusFailed
		res.Note = "no commands declared"
		res.AbortsServices = abortedSet(pf, cfg.ImpactedServices)
		return res, &FailureContext{Kind: "preflight", StepID: pf.ID, Command: "(none)"}
	}

	var lastStdout string
	for _, raw := range pf.Commands {
		// Preflights run before the publish tag is known and do not
		// reference services, so no substitution is needed; we still
		// pass an empty svc/tag for safety.
		cmd := substitute(raw, "", "")
		code, stdout, stderr, err := runShellCommand(ctx, cmd, cfg.Repo, pf.Cwd, allow, cfg.CmdTimeout, cfg.OnCommandLine)
		if err != nil {
			res.Status = StatusFailed
			res.Note = "exec error: " + truncateLine(err.Error(), 200)
			res.AbortsServices = abortedSet(pf, cfg.ImpactedServices)
			return res, &FailureContext{
				Kind: "preflight", StepID: pf.ID, Command: cmd,
				ExitCode: -1, Stdout: stdout, Stderr: err.Error(),
			}
		}
		if code != 0 {
			res.Status = StatusFailed
			res.Note = truncateLine(strings.TrimSpace(stderr), 400)
			res.AbortsServices = abortedSet(pf, cfg.ImpactedServices)
			return res, &FailureContext{
				Kind: "preflight", StepID: pf.ID, Command: cmd,
				ExitCode: code, Stdout: stdout, Stderr: stderr,
			}
		}
		lastStdout = stdout
	}

	// Every command exited zero — apply the SuccessCheck on the LAST
	// command's stdout (matches the convention in the YAML cheat sheet).
	trimmed := strings.TrimSpace(lastStdout)
	switch pf.SuccessCheck {
	case "", SuccessExitZero:
		// already validated by exit codes
	case SuccessStdoutEmpty:
		if trimmed != "" {
			res.Status = StatusFailed
			res.Note = fmt.Sprintf("expected empty stdout, got %q", truncateLine(trimmed, 120))
			res.AbortsServices = abortedSet(pf, cfg.ImpactedServices)
			return res, &FailureContext{
				Kind: "preflight", StepID: pf.ID,
				Command:  substitute(pf.Commands[len(pf.Commands)-1], "", ""),
				ExitCode: 0, Stdout: lastStdout,
				Stderr: "SuccessCheck=stdout_empty mismatch",
			}
		}
	case SuccessStdoutEquals:
		if trimmed != pf.OnFail.Expected {
			res.Status = StatusFailed
			res.Note = fmt.Sprintf("expected %q, got %q", pf.OnFail.Expected, truncateLine(trimmed, 120))
			res.AbortsServices = abortedSet(pf, cfg.ImpactedServices)
			return res, &FailureContext{
				Kind: "preflight", StepID: pf.ID,
				Command:  substitute(pf.Commands[len(pf.Commands)-1], "", ""),
				ExitCode: 0, Stdout: lastStdout,
				Stderr: fmt.Sprintf("SuccessCheck=stdout_equals expected=%q got=%q", pf.OnFail.Expected, trimmed),
			}
		}
	}

	res.Status = StatusPassed
	return res, nil
}

// runShellCommand parses a single command line, applies the allowlist,
// and runs it. Returns (exit_code, stdout, stderr, exec_err) where
// exec_err is non-nil ONLY for setup failures (allowlist denial,
// fork/exec error) — a process that ran and exited non-zero returns
// (code, _, _, nil).
//
// cwd is the working directory relative to repo (empty means the
// repo root). Used to honour PreflightStep.Cwd / ServiceSteps.Cwd
// so contracts like `cd services/api && govulncheck ./...` compile to
// (cwd="services/api", commands=["govulncheck ./..."]).
func runShellCommand(ctx context.Context, line, repo, cwd string, allow allowSets, timeout time.Duration, onLine func(stream, command, line string)) (int, string, string, error) {
	tokens := strings.Fields(line)
	if len(tokens) == 0 {
		return 0, "", "", errors.New("empty command")
	}
	program := tokens[0]
	if strings.ContainsAny(program, "/\\") {
		return 0, "", "", fmt.Errorf("program %q is a path, not a bare basename", program)
	}
	args := tokens[1:]
	var firstArg string
	if len(args) > 0 {
		firstArg = args[0]
	}
	if !allow.contains(program, firstArg) {
		return 0, "", "", fmt.Errorf("command %q (first arg %q) is not in the allowlist", program, firstArg)
	}
	// Defense in depth: reject any token containing shell metachars
	// even though Validate already did this — the cache could have
	// been written by an older Validate that missed something.
	const forbidden = "|&;$`<>\n\r"
	for _, a := range args {
		if strings.ContainsAny(a, forbidden) {
			return 0, "", "", fmt.Errorf("arg %q contains a forbidden shell metacharacter", a)
		}
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	workdir := repo
	if cwd != "" {
		workdir = filepath.Join(repo, cwd)
	}
	cmd := exec.CommandContext(cctx, program, args...)
	cmd.Dir = workdir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			// Process ran and exited non-zero — not a setup error.
			if onLine != nil {
				emitLines(stdout.String(), "stdout", program, onLine)
				emitLines(stderr.String(), "stderr", program, onLine)
			}
			return ee.ExitCode(), stdout.String(), stderr.String(), nil
		}
		return 0, stdout.String(), stderr.String(), err
	}
	if onLine != nil {
		emitLines(stdout.String(), "stdout", program, onLine)
		emitLines(stderr.String(), "stderr", program, onLine)
	}
	return 0, stdout.String(), stderr.String(), nil
}

func emitLines(s, stream, command string, onLine func(stream, command, line string)) {
	if s == "" {
		return
	}
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		onLine(stream, command, line)
	}
}

// substitute replaces <publish_tag> and <service> placeholders. An
// empty service / tag is allowed (preflights pass ""): the substitution
// then just removes the placeholder, which is fine because Validate
// has already refused <publish_tag> in preflight commands.
func substitute(s, service, publishTag string) string {
	s = strings.ReplaceAll(s, "<publish_tag>", publishTag)
	s = strings.ReplaceAll(s, "<service>", service)
	return s
}

// joinCommands flattens a series of command lists into a one-line
// representation suitable for the report's plan_or_published[].command
// field. Mirrors the "&&"-joined string today's agent emits in dry-run.
func joinCommands(lists ...[]string) string {
	var all []string
	for _, l := range lists {
		all = append(all, l...)
	}
	return strings.Join(all, " && ")
}

// remainingAfter returns the slice of names that come after `target`
// in ordered. Used to mark every downstream service as skipped when
// an upstream build/scan/push fails.
func remainingAfter(ordered []string, target string) []string {
	for i, s := range ordered {
		if s == target {
			return ordered[i+1:]
		}
	}
	return nil
}

// abortedSet returns the list of services this preflight blocks. For
// Mode=AbortAll it returns every impacted service (so the caller can
// fan the abort out); for Mode=AbortServices it returns the explicit
// list; for Mode=AbortWarn it returns an empty slice.
func abortedSet(pf PreflightStep, impacted []string) []string {
	switch pf.OnFail.Mode {
	case AbortAll:
		out := append([]string(nil), impacted...)
		sort.Strings(out)
		return out
	case AbortServices:
		out := append([]string(nil), pf.OnFail.AbortedServices...)
		sort.Strings(out)
		return out
	default:
		return []string{}
	}
}

// truncateLine clips s to max runes and replaces newlines with a
// visible glyph so a long, multi-line stderr stays on one report line.
func truncateLine(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ⏎ ")
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
