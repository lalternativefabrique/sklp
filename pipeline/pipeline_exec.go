// Package runner — pipeline compiler + executor bridge.
//
// This file used to be the runtime for `kind: pipeline` contracts: it
// parsed Pipeline{}, ran each step via `sh -c`, and managed env / scope
// gates / tool resolution inline. Engine 2.1 (#384) unifies execution
// on internal/plan, so the file now performs ONE job: compile a
// Pipeline{} into a plan.Plan{Kind: KindCI} and hand it to plan.ExecuteCI.
//
// Every runtime conditional that used to live here has moved to compile
// time:
//   - extends:        already merged by LoadPipeline.
//   - effective env:  layered into CIStep.Env at compile.
//   - tool versions:  pinned on the Plan; per-step subset frozen.
//   - scope / if:     resolved → StatusIncluded or StatusSkippedCompile.
//   - secrets:        presence-checked before compile.
//
// The runtime (plan.ExecuteCI) only has to: resolve a tool to a bin
// directory, prepend it to PATH, spawn `sh -c`. That gives us a single
// executor for both KindPublish and KindCI and unblocks the rest of
// the engine 2 epic (#382).
package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lalternativefabrique/sklp/pipeline/spec"

	"github.com/lalternativefabrique/sklp/internal/plan"
	"github.com/lalternativefabrique/sklp/internal/remotecache"
	"github.com/lalternativefabrique/sklp/internal/stackconfig"
	"github.com/lalternativefabrique/sklp/pipeline/reporter"
	"github.com/lalternativefabrique/sklp/pipeline/runnerimage"
)

// ErrPipelineFailed is returned when at least one Step exits non-zero.
// Kept exported because spawners and the CLI assert on it.
var ErrPipelineFailed = errors.New("pipeline failed")

// PipelineResult captures what happened during a pipeline run. The
// shape is unchanged from the pre-2.1 executor so JSON consumers
// (K8sJobSpawner, the sklp CLI, the web UI) keep parsing it as before.
type PipelineResult struct {
	Steps    []StepResult `json:"steps"`
	PassFail string       `json:"pass_fail"` // "pass" | "fail"
}

// StepResult is the per-step outcome.
type StepResult struct {
	Name       string `json:"name"`
	Status     string `json:"status"` // "passed" | "failed" | "skipped"
	ExitCode   int    `json:"exit_code,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

// PipelineRunOptions tweaks the executor. Fields documented in api.go's
// RunConfig — only the runtime-relevant subset lives here.
type PipelineRunOptions struct {
	RepoPath string
	BaseRef  string
	Stdout   io.Writer
	Stderr   io.Writer

	// NoCache, when true, bypasses the per-step content-addressed cache
	// (engine 2.3): every step runs even if a previous identical run is
	// on disk, and nothing is written. Mirrors `sklp run --no-cache`.
	NoCache bool

	// Force, when true, ignores the scope filter: every step is treated
	// as in-scope regardless of the `git diff` against BaseRef. Mirrors
	// the legacy `kind: task` -force flag for `kind: pipeline`. This is
	// orthogonal to NoCache — content-addressed cache hits still serve
	// from disk. Combine with NoCache for a full rebuild.
	Force bool

	// Only restricts the run to the steps naming one of these scopes,
	// bypassing the diff for them. See CompilePipelineOptions.Only.
	Only []string

	// Jobs caps the number of CI steps that may run concurrently.
	// 0 = the run's CPU budget (SKLP_CPUS, cgroup cpu.max, else
	// runtime.NumCPU()); 1 reproduces the pre-2.5 sequential
	// behaviour (engine 2.5).
	Jobs int

	// FailFast cancels every in-flight step on the first failure
	// (engine 2.5).
	FailFast bool

	// Resume short-circuits steps that were `passed` or `cached` in
	// the previous run and resumes execution at the first `failed` /
	// `skipped` (engine 2.7). Incompatible with NoCache.
	Resume bool

	// NoRemoteCache disables both remote lookup and remote push for
	// this run, regardless of whether SKLP_REMOTE_CACHE_URL is set.
	NoRemoteCache bool

	// Quiet, when true, suppresses live stdout/stderr streaming under
	// the spinner. The reporter's per-step status line is still
	// printed on StepFinished. Restores the pre-2.9 silent-spinner UX
	// for users who prefer it (engine 2.9).
	Quiet bool

	// TailLines / EnableHotkeys are #444 reporter-UX knobs. See
	// RunConfig for semantics; they are forwarded verbatim to
	// reporter.Options here.
	TailLines     int
	EnableHotkeys bool

	// TaskPath, when set, is the absolute path to the .sklp/tasks/*.yaml
	// file driving the run. Carried only so a require: violation banner
	// can reference the source of the rejected declaration; nothing in
	// the executor uses it for routing or caching. Empty is safe.
	TaskPath string

	// reporterOverride, when non-nil, is used instead of the default
	// reporter.New(stdout). Test seam only — production callers must
	// not set this field. Lives on the struct (rather than as a build
	// tag) so the test code does not need to fork the executor body.
	reporterOverride reporter.Reporter

	// Remote, when non-nil, overrides the env-derived remote cache
	// instance. Tests inject a fake here; production code leaves it
	// nil so the executor reads SKLP_REMOTE_CACHE_URL / _BUCKET.
	Remote plan.RemoteCache
}

// RunPipeline compiles p into a plan.Plan{Kind: KindCI} and runs it
// through plan.ExecuteCI. The function is the single bridge between
// the YAML schema (runner.Pipeline) and the deterministic executor
// (internal/plan).
//
// Pre-loop bail-outs (missing repo, missing secret, unknown tool name)
// return PipelineResult{PassFail: "fail"} so JSON consumers can tell
// an early failure from a successful zero-step run (#350).
func RunPipeline(ctx context.Context, p Pipeline, opts PipelineRunOptions) (PipelineResult, error) {
	failed := func(err error) (PipelineResult, error) {
		return PipelineResult{PassFail: "fail"}, err
	}
	if opts.RepoPath == "" {
		return failed(fmt.Errorf("%w: RepoPath is required", ErrInvalidConfig))
	}

	// 427: env_files loading — push dotenv contents into process env
	// BEFORE require: evaluation, tag resolution, or step compile. Host
	// env wins over file content so a CI runner that explicitly sets
	// GITHUB_TOKEN is never overridden by a checked-in `.env`.
	envStderr := opts.Stderr
	if envStderr == nil {
		envStderr = os.Stderr
	}
	// .sklp/config.yaml first, so an env_files entry of the same name still
	// wins locally — a developer overriding one value in .env.local should not
	// have to edit the committed config. Host env outranks both.
	if _, err := LoadPipelineStackConfig(opts.RepoPath, p.Secrets, envStderr); err != nil {
		return failed(err)
	}
	// The returned map is intentionally dropped: LoadPipelineEnvFiles has
	// already os.Setenv'd each key, which is what resolveSecrets reads.
	if _, err := LoadPipelineEnvFiles(p.EnvFiles, opts.RepoPath, envStderr); err != nil {
		return failed(err)
	}
	// env_files values are NOT folded into p.Env. p.Env lands on every
	// compiled step (compile_ci: cs.Env = merge(PipelineEnv, s.Env)), so
	// folding a dotenv there handed the whole file — GITHUB_TOKEN,
	// SCW_SECRET_KEY, every provider credential — to every step,
	// bypassing both the sandbox allowlist and the per-step `secrets:`
	// opt-in. A step that merely runs `pnpm install` then exposes the
	// full set to any postinstall script it executes.
	//
	// Instead the values reach a step through the same gate as any other
	// secret: resolveSecrets reads them below (LoadPipelineEnvFiles has
	// already os.Setenv'd each key), they travel as cfg.Secrets, and the
	// sandbox hands each step only the names it declared.

	// Re-wire git auth now that env_files are loaded into the process env
	// (LoadPipelineEnvFiles os.Setenv's each key). The early WireGitHubToken in
	// Run() fires BEFORE env_files exist, so a token that lives only in
	// .env/.env.local was invisible then and no insteadOf rewrite was written —
	// private fetches (go mod download of github.com/<org>/*) then clone
	// anonymously and 404. Wiring here, post-load, makes the rewrite reflect the
	// resolved token regardless of whether it came from the shell or a dotenv.
	if err := WireGitHubToken(envStderr); err != nil {
		fmt.Fprintf(envStderr, "[sklp] warning: wire GITHUB_TOKEN: %v\n", err)
	}

	// 412: require: block — evaluate preconditions BEFORE any side
	// effect. The invariant is load-bearing: a refused run must not
	// have compiled a step, started a service, resolved a tool, or
	// touched Docker. Only `git` is shelled out here.
	if err := EvaluateTrigger(ctx, p.On, opts.RepoPath); err != nil {
		return failed(err)
	}
	if err := EvaluateRequire(ctx, p.Require, opts.RepoPath); err != nil {
		return failed(err)
	}

	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	var rep reporter.Reporter
	if opts.reporterOverride != nil {
		rep = opts.reporterOverride
	} else {
		rep = reporter.New(stdout, reporter.Options{
			TailLines:     opts.TailLines,
			EnableHotkeys: opts.EnableHotkeys,
		})
	}
	logF, logPath := pipelineLogFile(opts.RepoPath, p.Name)
	defer logF.Close()

	// stepActive tracks whether a StepStarted line is currently on
	// screen. Read by the tool-log writer to decide whether SubAction
	// bullets attach to a step or fall through to the log file only.
	var (
		stepMu     sync.Mutex
		stepActive bool
	)

	rep.Header(fmt.Sprintf("sklp pipeline · %d step(s)", len(p.Steps)))
	if logPath != "" {
		fmt.Fprintln(logF, "# sklp pipeline log")
		fmt.Fprintln(logF, "# repo:", opts.RepoPath)
		fmt.Fprintln(logF, "# started:", time.Now().Format(time.RFC3339))
	}

	// --- Compile-time resolutions ------------------------------------

	// Secrets: fail-fast before any step (or service) runs. The plan
	// later carries the verified names for observability.
	var loginCovers map[string]bool
	if !plan.InCI() {
		loginCovers = registryPasswordVars(p)
	}
	resolvedSecrets, err := resolveSecrets(p.Secrets, opts.RepoPath, loginCovers)
	if err != nil {
		return failed(err)
	}

	resolvedRegs, err := resolvePushCredentials(p, plan.InCI(), defaultRegistryLoader, os.LookupEnv)
	if err != nil {
		return failed(err)
	}
	// One tracker for the whole run: a host reached by several publish steps
	// is logged into once.
	once := newLoginOnce()
	for _, reg := range resolvedRegs {
		if err := performRegistryLogin(reg, once, defaultRegistryLogin, stderr); err != nil {
			return failed(err)
		}
	}

	// The runner config's `runner.toolchain` (.sklp/space.yaml) is the
	// source of the execution ENV: the effective toolchain a pipeline run
	// sees is the UNION of that config and the pipeline's own `tools:`.
	// A pipeline tool with an explicit version overrides the config pin;
	// a config tool the pipeline doesn't mention is still part of the env
	// (a step may use it without redeclaring it). Computed once here, then
	// reused for compile, the per-step guardrail, and the resolver.
	// loadConfigToolchain returns nil on any error, so a missing or garbage
	// space.yaml degrades to "effective == p.Tools" (today's behaviour).
	configToolchain := loadConfigToolchain(opts.RepoPath)
	effective := mergeToolchain(p.Tools, configToolchain)

	// Compile the Plan. Scope evaluation is delegated to scopeMatches
	// (already exercised by every existing pipeline test), so the
	// compile path reuses production logic without a re-import.
	changed := spec.ChangedFiles(ctx, opts.RepoPath, opts.BaseRef)
	if len(p.Scopes) > 0 {
		fmt.Fprintf(logF, "[sklp] scope: %d changed file(s) vs base ref\n", len(changed))
	}
	compiled, err := CompilePipeline(p, CompilePipelineOptions{
		ChangedFiles: changed,
		RepoRoot:     opts.RepoPath,
		// 429: recursion check
		TaskPath:       opts.TaskPath,
		Warnings:       stderr,
		Force:          opts.Force,
		Only:           opts.Only,
		EffectiveTools: effective,
	})
	if err != nil {
		// Surface compile-time failures as a failed StepResult so JSON
		// consumers see "which step broke" instead of a bare error.
		var ce *plan.CICompileError
		if errors.As(err, &ce) && ce.Step != "" {
			res := PipelineResult{PassFail: "fail"}
			for _, s := range p.Steps {
				if s.Name == ce.Step {
					res.Steps = append(res.Steps, StepResult{Name: s.Name, Status: "failed"})
					return res, err
				}
				res.Steps = append(res.Steps, StepResult{Name: s.Name, Status: "skipped"})
			}
		}
		return failed(err)
	}

	// Validate every step's `tools:` against the effective toolchain
	// (pipeline `tools:` ∪ config `runner.toolchain`). Done after compile
	// so a step may name any tool the execution env provides.
	for _, step := range p.Steps {
		if _, err := stepTools(step, effective); err != nil {
			return failed(err)
		}
	}

	// --- Run-time scaffolding (reporter, tool resolver, services) ----

	cacheDir, err := ensureToolsCache()
	if err != nil {
		return failed(err)
	}
	toolLog := newToolLogWriter(rep, logF, stderr, &stepMu, &stepActive)
	// effective (pipeline ∪ config toolchain) was computed above, before
	// compile, so the resolver, the guardrail and the plan all agree.
	resolver := newToolResolver(effective, cacheDir, opts.RepoPath, toolLog)

	// The in-process sandbox (internal/plan) filters BaseEnv through
	// its allowlist (PATH, HOME, USER, TZ, LANG, LC_ALL). Pipeline env
	// is already on each CIStep.Env via CompileCI; resolved secrets
	// flow through cfg.Secrets and are exposed only to steps that
	// opt-in via `secrets:`.
	baseEnv := os.Environ()

	// Only boot the services referenced by steps that survived the
	// scope filter. Without this, a 100%-frontend diff would still
	// spin up postgres + nats because a backend step (gated out at
	// compile) declared them — slow, racy, and surfaces "service not
	// ready" errors on runs that had nothing to do.
	includedSteps := make(map[string]bool, len(compiled.CI.Steps))
	for _, s := range compiled.CI.Steps {
		if s.Status == plan.StatusIncluded {
			includedSteps[s.Name] = true
		}
	}
	servicesTeardown, _, err := startServicesForPipeline(ctx, p, includedSteps, opts.RepoPath, logF, logF)
	if err != nil {
		return failed(fmt.Errorf("start services: %w", err))
	}
	defer servicesTeardown()

	// State threaded through the per-step event callbacks. Each step's
	// stats live here so the OnStepStart hook can create them and
	// OnStepEnd can finalise the reporter / FAILURES.
	type stepState struct {
		stats      *stepStats
		stderrTail *bytes.Buffer
		lineW      *stepLineWriter
		// streamTees, when streaming is enabled, are the LineTee
		// wrappers that feed reporter.StepLine. Held so OnStepEnd can
		// flush any trailing partial line into the reporter before
		// the step's frozen status line is printed.
		streamTees []*reporter.LineTee
	}
	states := make(map[string]*stepState, len(p.Steps))
	stepByName := make(map[string]Step, len(p.Steps))
	for _, s := range p.Steps {
		stepByName[s.Name] = s
	}

	result := PipelineResult{Steps: make([]StepResult, 0, len(p.Steps))}
	overallStart := time.Now()
	// failuresMu guards allFailures + firstFailure under multi-step
	// concurrency. The OnStepEnd callback fires from worker goroutines
	// when LiveReporter is set, so the post-run aggregation below must
	// see a coherent snapshot.
	var failuresMu sync.Mutex
	var allFailures []reporter.Failure
	var failedStepIdx int = -1
	var firstFailure *plan.CIStepEvent

	// Reporter-capability probe. A multi-step-capable reporter (the
	// live PrettyReporter) accepts concurrent StepStarted / StepFinished
	// calls, so the scheduler can stop pinning a single "current step"
	// and the runner can hand the engine dedicated per-step writers.
	_, multiStep := rep.(reporter.MultiStepReporter)

	// Streamer probe: pretty reporters expose a StepStreamer capability
	// to receive line-by-line stdout/stderr while a step is still
	// running. Disabled when the caller passed -quiet (engine 2.9).
	streamer, hasStreamer := rep.(reporter.StepStreamer)
	streamingOn := hasStreamer && !opts.Quiet
	if streamingOn {
		if es, ok := rep.(interface{ EnableStreaming() }); ok {
			es.EnableStreaming()
		}
	}

	// statesMu guards states + the live-mode writer that StepFinished
	// looks up by name (the engine may finish steps out of order under
	// parallel scheduling).
	var statesMu sync.Mutex

	// makeWriter builds the per-step writers and stashes them in states
	// keyed by step name. Returns (stdout, stderr) writers for the CI
	// executor to pipe sh -c into.
	makeWriter := func(name string) (io.Writer, io.Writer) {
		st := &stepState{
			stats:      &stepStats{},
			stderrTail: &bytes.Buffer{},
		}
		// Live mode: step lines go to the log file only. The trunk
		// terminal is owned by the PrettyReporter's live block and
		// scrolling the step's stdout there would corrupt it. With
		// streaming enabled the user still sees a rolling tail under
		// the spinner via reporter.StepLine; with -quiet, only the
		// per-step result line via StepFinished.
		liveSink := stdout
		if multiStep || streamingOn {
			liveSink = io.Discard
		}
		st.lineW = newStepLineWriter(logF, liveSink, st.stats, st.stderrTail)
		statesMu.Lock()
		states[name] = st
		statesMu.Unlock()
		var sw io.Writer = st.lineW
		var ew io.Writer = st.lineW
		if streamingOn {
			// Wrap each writer in a LineTee so reporter.StepLine
			// receives each complete line. The tee forwards every
			// byte to the underlying writer first, so the log file
			// + filter chain stay byte-identical to the -quiet path.
			//
			// We drop on a full / slow reporter by virtue of the
			// reporter itself holding a non-blocking mutex; lines
			// for a step that already finished are filtered in
			// PrettyReporter.StepLine.
			stepName := name
			soutTee := &reporter.LineTee{
				Underlying: sw,
				Emit: func(line string) {
					streamer.StepLine(stepName, line, false)
				},
			}
			serrTee := &reporter.LineTee{
				Underlying: ew,
				Emit: func(line string) {
					streamer.StepLine(stepName, line, true)
				},
			}
			st.streamTees = []*reporter.LineTee{soutTee, serrTee}
			return soutTee, serrTee
		}
		return sw, ew
	}

	// stepStdout / stepStderr point at the writers of the currently
	// running step. plan.ExecuteCI receives one Stdout / Stderr for the
	// whole run, so we wrap it in a tiny dispatcher that forwards to
	// the active step's writer. OnStepStart swaps the target before sh
	// is spawned, OnStepEnd drains the line writer.
	var (
		stepWMu  sync.Mutex
		curStdW  io.Writer = io.Discard
		curStdE  io.Writer = io.Discard
		curStepN string
	)
	dispatchStdout := writerFunc(func(b []byte) (int, error) {
		stepWMu.Lock()
		w := curStdW
		stepWMu.Unlock()
		return w.Write(b)
	})
	dispatchStderr := writerFunc(func(b []byte) (int, error) {
		stepWMu.Lock()
		w := curStdE
		stepWMu.Unlock()
		return w.Write(b)
	})

	// Engine 2.3: open the per-step content-addressed cache and derive
	// the plan key. A failure here is non-fatal — we just disable the
	// cache for this run and let the executor spawn every step.
	stepCache := plan.OpenStepCache(opts.RepoPath)
	stepCAS := plan.OpenCAS(opts.RepoPath)
	planKey, planKeyErr := plan.ComputeCIPlanKey(compiled, opts.RepoPath)
	if planKeyErr != nil {
		fmt.Fprintf(logF, "[sklp] cache: disabled (plan key: %v)\n", planKeyErr)
		stepCache = nil
		stepCAS = nil
	}

	// Engine 2.7: surface what `--resume` is going to do BEFORE the
	// executor runs so the user sees the routing decision in the
	// terminal (not just in the per-step log file).
	if opts.Resume && stepCache != nil && planKey != "" {
		switch planResumeStatus(opts.RepoPath, planKey) {
		case resumeStatusFound:
			fmt.Fprintf(stderr, "[sklp] resume: replaying cached passes from previous run\n")
		case resumeStatusKeyChanged:
			fmt.Fprintf(stderr, "[sklp] resume: plan key changed since last run; resume bypassed\n")
		case resumeStatusFirstRun:
			fmt.Fprintf(stderr, "[sklp] resume: no previous run found for this plan; running normally\n")
		}
	}

	// Engine 2.8 (epic #400): remote cache. Resolved here so the
	// executor can transparently pull / push deterministic step
	// results across machines. Disabled by --no-remote-cache, by
	// NoCache (which already implies no caching at all), or when the
	// SKLP_REMOTE_CACHE_URL env var is unset.
	var remote plan.RemoteCache
	if !opts.NoRemoteCache && !opts.NoCache && stepCache != nil {
		if opts.Remote != nil {
			remote = opts.Remote
		} else {
			rc, rerr := remotecache.New(ctx)
			if rerr != nil {
				fmt.Fprintf(stderr, "[sklp] remote cache: disabled (%v)\n", rerr)
			} else if rc != nil {
				remote = newRemoteCacheAdapter(rc, opts.RepoPath)
				st := rc.Status()
				fmt.Fprintf(logF, "[sklp] remote cache: enabled (endpoint=%s bucket=%s)\n", st.Endpoint, st.Bucket)
			}
		}
	}

	cfg := plan.CIExecConfig{
		Plan:          compiled,
		Repo:          opts.RepoPath,
		ToolsCacheDir: cacheDir,
		BaseEnv:       baseEnv,
		Secrets:       resolvedSecrets,
		Stdout:        dispatchStdout,
		Stderr:        dispatchStderr,
		Cache:         stepCache,
		CAS:           stepCAS,
		PlanKey:       planKey,
		NoCache:       opts.NoCache,
		Jobs:          opts.Jobs,
		FailFast:      opts.FailFast,
		Resume:        opts.Resume,
		Remote:        remote,
		LiveReporter:  multiStep,
		ResolveTool: func(ctx context.Context, name string) (string, error) {
			return resolver.Get(ctx, name)
		},
		// MakeStepWriters: under multi-step the scheduler must route
		// each step's shell output to a dedicated, name-keyed writer
		// (no single "current step" slot). For the pre-live path we
		// keep nil so the scheduler falls back to its own buffered
		// flush + the dispatchStdout/dispatchStderr swap dance below.
		MakeStepWriters: func() func(string) (io.Writer, io.Writer) {
			if !multiStep {
				return nil
			}
			return func(name string) (io.Writer, io.Writer) {
				return makeWriter(name)
			}
		}(),
		OnStepStart: func(ev plan.CIStepEvent) {
			if !multiStep {
				stepMu.Lock()
				stepActive = true
				stepMu.Unlock()
			}
			rep.StepStarted(ev.Name)

			// Log the requested services (mirrors pre-2.1 behaviour).
			if st, ok := stepByName[ev.Name]; ok && len(st.Services) > 0 {
				fmt.Fprintf(logF, "[sklp] step %q requests services: [%s]\n",
					st.Name, strings.Join(st.Services, " "))
			}

			// Sequential path: build per-step writers here and pin
			// them via the dispatcher. Multi-step path already wired
			// dedicated writers via MakeStepWriters at run time.
			if !multiStep {
				sw, ew := makeWriter(ev.Name)
				stepWMu.Lock()
				curStdW = sw
				curStdE = ew
				curStepN = ev.Name
				stepWMu.Unlock()
			}
		},
		OnStepEnd: func(ev plan.CIStepEvent) {
			name := ev.Name
			if !multiStep {
				stepWMu.Lock()
				if curStepN != "" {
					name = curStepN
				}
				stepWMu.Unlock()
			}
			statesMu.Lock()
			st, ok := states[name]
			statesMu.Unlock()
			if !ok {
				// Compile-skipped or previous-failure skip: we never
				// allocated a state. Render the skip line and move on.
				if ev.Status == plan.StatusSkipped {
					reason := ev.SkipReason
					if reason == "" {
						reason = "skipped"
					}
					rep.StepFinished(ev.Name, reporter.StatusSkipped, 0, reason)
				}
				return
			}
			for _, t := range st.streamTees {
				t.Flush()
			}
			st.lineW.Flush()

			stepMu.Lock()
			stepActive = false
			stepMu.Unlock()

			// Tag any FAIL blocks with the step name so the FAILURES
			// section reads `<step> › <test>`.
			for j := range st.stats.failures {
				st.stats.failures[j].Scope = name
			}
			failuresMu.Lock()
			allFailures = append(allFailures, st.stats.failures...)
			failuresMu.Unlock()

			// emitTail prints the last 5 non-empty lines of the step's
			// combined stdout/stderr right under the recap line, as dim
			// SubAction bullets. Uniform UX across go-test, DAST scans
			// (ZAP / nuclei), shell pipelines, etc. — the operator never
			// has to grep .sklp/logs/ to see the verdict line.
			// See issue d3eba457-04a4-4496-917e-080ea103fdf1.
			emitTail := func() {
				for _, ln := range st.lineW.Tail() {
					rep.SubAction(ln)
				}
			}

			switch ev.Status {
			case plan.StatusPassed:
				rep.StepFinished(name, reporter.StatusPassed, ev.Duration, st.stats.summary())
				emitTail()
			case plan.StatusCached:
				summary := st.stats.summary()
				if summary == "" {
					summary = ev.Note
				}
				rep.StepFinished(name, reporter.StatusCached, ev.Duration, summary)
				emitTail()
			case plan.StatusFailed:
				summary := st.stats.summary()
				if summary == "" {
					summary = ev.Note
				}
				rep.StepFinished(name, reporter.StatusFailed, ev.Duration, summary)
				emitTail()
				failuresMu.Lock()
				if len(st.stats.failures) == 0 {
					// publish / Trivy steps produce a wide ASCII
					// table that columns are encoded in byte positions.
					// We pull the FULL captured stream (not just the
					// last 8 lines) and ask the reporter to render it
					// byte-faithful. #444 / #440: without this, the
					// FAILURES section showed a column of empty `|`
					// because tailLines(…, 8) only caught the table's
					// separator rows.
					kind := stepByName[name].Kind
					preserve := kind == string(plan.KindCIStepPublish)
					var tail string
					if preserve {
						tail = strings.TrimRight(st.stderrTail.String(), "\n")
					} else {
						tail = tailLines(st.stderrTail.String(), 8)
					}
					title := ev.Note
					if title == "" {
						title = "step failed"
					}
					// When the step failed during the implicit auto-install
					// (pnpm install, go mod download, …), the wrapper error
					// returned by ResolveTool is just "exit status N" — the
					// real cause (pnpm's "ERR_PNPM_…" block, etc.) is only
					// in the log file. Pull the captured tail so the
					// FAILURES section shows it inline.
					message := tail
					if fail := resolver.ConsumeLastAutoInstallFailure(); fail != nil {
						message = formatAutoInstallFailure(fail, logPath)
						preserve = false
					}
					allFailures = append(allFailures, reporter.Failure{
						Scope:         name,
						Title:         title,
						Message:       message,
						PreserveBytes: preserve,
					})
				}
				if firstFailure == nil {
					ev2 := ev
					firstFailure = &ev2
				}
				failuresMu.Unlock()
			case plan.StatusSkipped:
				reason := ev.SkipReason
				if reason == "" {
					reason = "skipped"
				}
				rep.StepFinished(name, reporter.StatusSkipped, 0, reason)
			}

			if !multiStep {
				stepWMu.Lock()
				curStdW = io.Discard
				curStdE = io.Discard
				curStepN = ""
				stepWMu.Unlock()
			}
		},
	}

	ciRep, runErr := plan.ExecuteCI(ctx, cfg)

	// ExecuteCI returns (nil, err) on early failures (nil plan, bad
	// PlanKey, EACCES on .sklp/cache/plans/, etc.). The translation loop
	// below dereferences ciRep, so a missing guard turns any of those
	// errors into a SIGSEGV. Report the error verbatim instead so the
	// operator sees the real cause (e.g. "permission denied" on the
	// cache) rather than a Go stack trace.
	if ciRep == nil {
		if runErr == nil {
			runErr = errors.New("ExecuteCI returned a nil report without an error")
		}
		return PipelineResult{PassFail: "fail"}, runErr
	}

	// Translate the CI report back into a PipelineResult. The mapping
	// is 1:1 because plan.CIStepResult was modelled on StepResult.
	for i, r := range ciRep.Steps {
		stepRes := StepResult{
			Name:       r.Name,
			Status:     r.Status,
			ExitCode:   r.ExitCode,
			DurationMS: r.DurationMS,
		}
		if r.Status == "failed" && failedStepIdx == -1 {
			failedStepIdx = i
		}
		result.Steps = append(result.Steps, stepRes)
	}
	result.PassFail = ciRep.PassFail

	totalDur := time.Since(overallStart)
	if failedStepIdx >= 0 || runErr != nil {
		if result.PassFail == "" {
			result.PassFail = "fail"
		}
		if len(allFailures) > 0 {
			rep.Failures("FAILURES", allFailures)
		}
		fields := []reporter.FinalField{}
		if logPath != "" {
			fields = append(fields, reporter.FinalField{Label: "log", Value: logPath})
		}
		rep.Finalize("CI", reporter.StatusFailed, totalDur, fields, nil)
		toolLog.Flush()

		if runErr != nil {
			return result, runErr
		}
		failedName := result.Steps[failedStepIdx].Name
		return result, fmt.Errorf("%w: step %q (exit %d)", ErrPipelineFailed, failedName, result.Steps[failedStepIdx].ExitCode)
	}

	fields := []reporter.FinalField{}
	if logPath != "" {
		fields = append(fields, reporter.FinalField{Label: "log", Value: logPath})
	}
	rep.Finalize("CI", reporter.StatusPassed, totalDur, fields, nil)
	toolLog.Flush()
	return result, nil
}

// loadConfigToolchain reads `.sklp/space.yaml` and returns its
// `runner.toolchain` map, or nil on ANY error (missing file, read error,
// parse error). The swallow-to-nil is deliberate: `sklp runner build`
// already surfaces config parse errors loudly to the user, whereas
// `sklp run` must degrade gracefully — a malformed or absent space.yaml
// must never turn a runnable pipeline into a hard failure, it just falls
// back to the pipeline's own `tools:` versions (mirrors the tolerant
// precedent in internal/repofacts/registry.go). We reuse runnerimage.Load
// rather than writing a second parser; runnerimage does not import runner,
// so the dependency is cycle-free.
func loadConfigToolchain(repoPath string) map[string]string {
	cfgPath := runnerimage.ResolvePath(repoPath)
	cfg, err := runnerimage.Load(cfgPath)
	if err != nil {
		return nil
	}
	out := make(map[string]string, len(cfg.Runner.Toolchain)+len(cfg.Runner.GoTools))
	for k, v := range cfg.Runner.Toolchain {
		out[k] = v
	}
	// go_tools binaries are baked into PATH; expose their names so steps can
	// reference them in `tools:` without a separate pipeline-level declaration.
	// Keep the pinned version: skt requires an explicit version, so blanking it
	// here makes any step using one of these tools fail at resolve time.
	for _, mod := range cfg.Runner.GoTools {
		if spec, _, ok := runnerimage.GoToolSktSpec(mod); ok {
			// spec is "name@ver"; carry both so the resolver can pin it.
			name, ver, _ := strings.Cut(spec, "@")
			out[name] = ver
		}
	}
	return out
}

// CompilePipelineOptions configures CompilePipeline. Split out so the
// public function signature stays stable as future compile-time
// resolutions (DAG, content-addressed cache key) get added.
type CompilePipelineOptions struct {
	// ChangedFiles is the pre-computed list of files changed in the
	// current run, used to resolve scope gates. Nil means "no
	// information → run every scoped step" (degraded-open).
	ChangedFiles []string

	// RepoRoot is the absolute path the compiler uses to resolve a
	// step's input globs into a concrete sorted file list. Empty
	// disables input resolution (the compiler stores the declared
	// globs but leaves ResolvedInputs nil).
	RepoRoot string

	// 429: recursion check — TaskPath is the source .sklp/tasks/*.yaml
	// file driving the compile, surfaced in the recursion warning so
	// the user knows which file to fix. Empty disables the path prefix
	// (the warning still fires, with "<unknown>" as the source).
	TaskPath string

	// 429: recursion check — Warnings is the io.Writer receiving
	// compile-time warnings (scope/output recursion today, possibly
	// more later). nil disables warning emission entirely; tests pass
	// a bytes.Buffer here to assert on the exact wording.
	Warnings io.Writer

	// Force, when true, makes the compiler treat every scoped step as
	// in-scope regardless of ChangedFiles. Orthogonal to the per-step
	// content-addressed cache: a step whose inputs are byte-identical to
	// a previous run is still served from cache. Mirrors -force on the
	// runner CLI for `kind: pipeline` (same semantics as the legacy
	// `kind: task` path's applyForce, but applied at the scope filter
	// instead of the resolved-plan rewrite).
	Force bool

	// Only, when non-empty, restricts the run to the steps whose `scope:`
	// names at least one of these scopes; every other step compiles to
	// StatusSkippedCompile. Within the selected set the diff no longer
	// gates anything — naming a scope IS the intent to run it — so Only
	// implies Force for those steps. An unscoped step is never selected:
	// it has no scope to name, and silently running it would contradict
	// the flag. Names are validated against `scopes:` by the caller.
	Only []string

	// EffectiveTools is the toolchain the run will actually expose:
	// the pipeline's `tools:` merged with the runner config's
	// `runner.toolchain` (space.yaml). Used so a step with no `tools:`
	// inherits the full execution env, and a step may name a config-only
	// tool without redeclaring it. Nil falls back to the pipeline's own
	// `tools:` (callers that don't load config — e.g. unit tests).
	EffectiveTools map[string]string
}

// CompilePipeline translates a Pipeline (parsed YAML, already
// extends-merged by LoadPipeline) into a plan.Plan{Kind: KindCI}.
//
// Every runtime conditional in pipeline_exec used to host moves here:
// scope gates, env merging, tool subset selection, `if:` rejection.
// The resulting Plan is fully resolved and JSON-serialisable; the
// executor (plan.ExecuteCI) only has to spawn `sh -c`.
// Cache-input precedence (see issue feat/scope-as-cache-inputs):
//  1. If a step declares `inputs:` explicitly, those globs are the
//     SOLE source of cache invalidation — explicit always wins.
//  2. Else if the step declares `scope:`, the union of the named
//     scopes' globs becomes the implicit inputs. This makes `scope:`
//     the single source of truth for both filtering (skip when no
//     changed file matches) AND cache invalidation (hash the files
//     that match those globs into the step key). Without this rule,
//     a step with only `scope:` would silently serve a stale cache
//     after a real source change because the cache key would ignore
//     the working tree.
//  3. Else (no inputs, no scope) the step is input-independent — its
//     cache key folds in the empty-inputs digest, same as before.
func CompilePipeline(p Pipeline, opts CompilePipelineOptions) (*plan.Plan, error) {
	// 429: recursion check — emit a warning (never fail) for each
	// declared output that matches one of the pipeline's own scope
	// globs. Runs before any side effect so the user sees the hint
	// even if compile aborts further down for another reason.
	taskPath := opts.TaskPath
	if taskPath == "" {
		taskPath = "<unknown>"
	}
	checkScopeOutputRecursion(p, taskPath, opts.Warnings)

	// 413: resolve the publish tag once, up-front. A failure here is a
	// compile error — every downstream step would otherwise see an
	// unresolved `{{ publish_tag }}` and the cache key would lie about
	// what was actually built.
	publishTag, tagFacts, err := resolvePipelineTag(p, opts.RepoRoot)
	if err != nil {
		return nil, &plan.CICompileError{Reason: err.Error(), Cause: err}
	}

	steps := make([]plan.CIStepInput, 0, len(p.Steps))
	for _, s := range p.Steps {
		// 413 + 411 fix: substitute `{{ publish_tag }}` (and the other
		// tag-facts placeholders) into the structured publish
		// fields BEFORE deriving cache inputs from Context/Dockerfile.
		// PR #413 wired substitution for Run/Env only; PR #411 added
		// these fields in parallel and they were missed. Without this,
		// `tag: "{{ publish_tag }}"` reaches `docker build -t` as a
		// literal and produces "invalid reference format".
		image := s.Image
		tag := s.Tag
		buildCtx := s.Context
		dockerfile := s.Dockerfile
		if publishTag != "" {
			image = substituteTagPlaceholders(image, publishTag, tagFacts)
			tag = substituteTagPlaceholders(tag, publishTag, tagFacts)
			buildCtx = substituteTagPlaceholders(buildCtx, publishTag, tagFacts)
			dockerfile = substituteTagPlaceholders(dockerfile, publishTag, tagFacts)
		}

		inputs := s.Inputs
		if len(inputs) == 0 && len(s.Scope) > 0 {
			expanded, err := spec.ExpandScopesToGlobs(p.Scopes, []string(s.Scope))
			if err != nil {
				return nil, &plan.CICompileError{Step: s.Name, Reason: err.Error()}
			}
			inputs = expanded
		}
		// 411: publish — when the user does not declare explicit
		// inputs, derive them from the build context so the cache key
		// invalidates on a Dockerfile edit or any source change under
		// the context directory. This is what makes "rebuild same
		// source → cached" work.
		//
		// We walk the context HONOURING .dockerignore (+ always skipping .git
		// and .claude) instead of emitting a bare `<ctx>/**` glob: a naive glob
		// descends into node_modules, .git, and stray .claude/worktrees trees —
		// hundreds of thousands of files — making the pre-build input scan take
		// minutes (and bloating the cache key with content that never ships).
		if len(inputs) == 0 && s.Kind == string(plan.KindCIStepPublish) && buildCtx != "" {
			df := dockerfile
			if df == "" {
				df = buildCtx + "/Dockerfile"
			}
			ctxInputs, err := spec.DockerContextInputs(opts.RepoRoot, buildCtx, df)
			if err != nil {
				return nil, &plan.CICompileError{Step: s.Name, Reason: err.Error()}
			}
			inputs = ctxInputs
		}
		run := s.Run
		env := s.Env
		if publishTag != "" {
			run = substituteTagPlaceholders(run, publishTag, tagFacts)
			env = substituteEnvPlaceholders(env, publishTag, tagFacts)
		}
		// 421: BuildKit secrets / build args for `kind: publish`.
		// The schema parses `secrets:` into either a shell-step name
		// allowlist OR a publish BuildKit list. Reject the mismatch
		// up-front so the user sees a precise error instead of "unknown
		// secret name" at the executor.
		secretNames := s.Secrets.Names
		buildSecrets := s.Secrets.Build
		if s.Kind != string(plan.KindCIStepPublish) && len(buildSecrets) > 0 {
			return nil, &plan.CICompileError{
				Step:   s.Name,
				Reason: "BuildKit secret entries (id/env/file) are only valid on `kind: publish` steps",
			}
		}
		if s.Kind == string(plan.KindCIStepPublish) && len(secretNames) > 0 {
			return nil, &plan.CICompileError{
				Step:   s.Name,
				Reason: "`kind: publish` `secrets:` entries must be {id, env|file} mappings, not bare names",
			}
		}

		// build_args: substitute placeholders in values now, before the
		// cache key sees them. Non-secret by contract — values surface in
		// the docker history.
		//
		// Order matters:
		//   1. Validate keys (cheap, no env touched).
		//   2. Expand `{{ publish_tag }}` and tag-facts placeholders
		//      (cache-deterministic templating).
		//   3. Expand `${VAR}` env references against the layered env
		//      (env_files → pipeline.Env → step.Env → process env), so
		//      a tag-fact whose literal contains `${...}` is NOT
		//      re-expanded as an env ref.
		buildArgs := s.BuildArgs
		if err := validateBuildArgKeys(s.Name, buildArgs); err != nil {
			return nil, &plan.CICompileError{Step: s.Name, Reason: err.Error()}
		}
		if publishTag != "" && len(buildArgs) > 0 {
			out := make(map[string]string, len(buildArgs))
			for k, v := range buildArgs {
				out[k] = substituteTagPlaceholders(v, publishTag, tagFacts)
			}
			buildArgs = out
		}
		expandedArgs, err := interpolateBuildArgs(s.Name, buildArgs, p.Env, s.Env)
		if err != nil {
			var cause *BuildArgEnvMissingError
			if errors.As(err, &cause) {
				return nil, &plan.CICompileError{
					Step:   s.Name,
					Reason: cause.Error(),
					Cause:  cause,
				}
			}
			return nil, &plan.CICompileError{Step: s.Name, Reason: err.Error()}
		}
		buildArgs = expandedArgs

		// 445: build_contexts — substitute placeholders + fold local-dir
		// sources into Inputs so the cache key invalidates on tree
		// changes. BuildKit treats the value as opaque; sklp only
		// intervenes here for placeholder expansion and cache hashing.
		// Image / git / OCI refs flow through unchanged and enter the
		// cache key via the spec's JSON marshalling.
		buildContexts := s.BuildContexts
		if publishTag != "" && len(buildContexts) > 0 {
			out := make(map[string]string, len(buildContexts))
			for k, v := range buildContexts {
				out[k] = substituteTagPlaceholders(v, publishTag, tagFacts)
			}
			buildContexts = out
		}
		// Local-dir sources extend the auto-derived inputs glob set. We
		// only do this when the user did not declare explicit Inputs —
		// the same gate as the Context auto-derivation above.
		//
		// Out-of-repo paths (e.g. `../platform`) are intentionally NOT
		// folded into Inputs: the runner's input resolver rejects globs
		// that escape the repo root. Such build_contexts entries still
		// enter the cache key via PublishSpec.BuildContexts (the
		// value string), so changing the *path* invalidates the cache;
		// editing files at the path does NOT, the same trade-off as a
		// floating image tag. Users who need that signal should arrange
		// for the dependency to live inside the repo (vendoring,
		// submodule, etc.).
		if len(s.Inputs) == 0 && s.Kind == string(plan.KindCIStepPublish) && len(buildContexts) > 0 {
			for _, v := range buildContexts {
				path, ok := localDirBuildContext(v)
				if !ok || pathEscapesRepoRoot(path) {
					continue
				}
				inputs = append(inputs, path+"/**")
			}
		}

		// Fail-fast: an `env:` secret entry that references a missing
		// runner env var becomes a docker error 30s into the build. The
		// message format is part of the user contract (issue #421).
		for _, sec := range buildSecrets {
			if sec.Env == "" {
				continue
			}
			if _, ok := os.LookupEnv(sec.Env); !ok {
				cause := &BuildKitSecretEnvMissingError{
					Step:     s.Name,
					SecretID: sec.ID,
					EnvVar:   sec.Env,
				}
				return nil, &plan.CICompileError{
					Step:   s.Name,
					Reason: cause.Error(),
					Cause:  cause,
				}
			}
		}

		// Tilde-expand `file:` paths so the executor sees a real path.
		// File existence is left to docker — a missing file there is a
		// clearer error than a stat() race here.
		planBuildSecrets := make([]plan.PublishSecret, 0, len(buildSecrets))
		for _, sec := range buildSecrets {
			file := sec.File
			if file != "" {
				file = expandTilde(file)
			}
			planBuildSecrets = append(planBuildSecrets, plan.PublishSecret{
				ID:   sec.ID,
				Env:  sec.Env,
				File: file,
			})
		}

		steps = append(steps, plan.CIStepInput{
			Name:       s.Name,
			Run:        run,
			If:         s.If,
			Env:        env,
			Scope:      []string(s.Scope),
			Tools:      s.Tools,
			Services:   s.Services,
			DependsOn:  s.DependsOn,
			Cwd:        s.Cwd,
			Network:    s.Network,
			Filesystem: s.Filesystem,
			Secrets:    secretNames,
			Inputs:     inputs,
			Outputs:    s.Outputs,
			Cache:      s.Cache,
			// 411: publish — propagate the wrapper config so
			// CompileCI can resolve a PublishSpec.
			Kind:            s.Kind,
			Image:           image,
			Tag:             tag,
			Context:         buildCtx,
			Dockerfile:      dockerfile,
			Push:            s.Push,
			ScanPresent:     s.Scan.Present,
			ScanDisabled:    s.Scan.Disabled,
			ScanSeverity:    s.Scan.Severity,
			ScanIgnoreUnfix: s.Scan.IgnoreUnfixed,
			// 421: BuildKit secrets and build args.
			BuildArgs:    buildArgs,
			BuildSecrets: planBuildSecrets,
			// 445: BuildKit additional contexts.
			BuildContexts: buildContexts,
			Platform:      s.Platform,
		})
	}

	// Pipeline-level env: layer SKLP_PUBLISH_TAG on top when the tag
	// block is declared. Step-level env still wins on conflict (the
	// general overlay rule); a user that explicitly sets the same key
	// keeps full control.
	pipelineEnv := p.Env
	if publishTag != "" {
		pipelineEnv = mergeStringMaps(p.Env, map[string]string{PublishTagEnvVar: publishTag})
	}

	// The effective toolchain (pipeline ∪ config) drives per-step tool
	// selection: a `Tools == nil` step inherits the full env, and an
	// explicit list may reference config-only tools. Falls back to the
	// pipeline's own `tools:` when the caller didn't supply it.
	pipelineTools := opts.EffectiveTools
	if pipelineTools == nil {
		pipelineTools = p.Tools
	}

	in := plan.CompileCIInput{
		Steps:         steps,
		PipelineEnv:   pipelineEnv,
		PipelineTools: pipelineTools,
		Secrets:       p.Secrets,
		ChangedFiles:  opts.ChangedFiles,
		Scopes:        p.Scopes,
	}
	matcher := spec.ScopeMatches
	if opts.Force {
		// Bypass the changed-files filter: every scope is in-scope. Does
		// NOT bypass the content-addressed cache — pair with -no-cache
		// for a real rebuild.
		matcher = forceScopeMatches
	}
	if len(opts.Only) > 0 {
		if err := validateOnlyScopes(opts.Only, p.Scopes); err != nil {
			return nil, &plan.CICompileError{Reason: err.Error(), Cause: err}
		}
		matcher = onlyScopeMatches(opts.Only)
		// CompileCI only consults the matcher for a step that declares a
		// scope, so an unscoped step would slip through the selection.
		// Tag those steps with a sentinel scope the matcher always
		// rejects, making them reachable by the gate.
		for i := range in.Steps {
			if len(in.Steps[i].Scope) == 0 {
				in.Steps[i].Scope = []string{unscopedSentinel}
			}
		}
	}
	return plan.CompileCI(in, matcher, makeInputResolver(opts.RepoRoot))
}

// forceScopeMatches is the -force scope matcher: every step is in-scope
// regardless of the diff. Signature matches scopeMatches.
func forceScopeMatches(_ []string, _ map[string][]string, _ []string) (bool, error) {
	return true, nil
}

// unscopedSentinel is the scope name stamped on scope-less steps under
// -only so they reach the scope gate and get rejected. It is not a valid
// YAML scope name, so it can never collide with a declared scope nor be
// spelled on the command line.
const unscopedSentinel = "\x00unscoped"

// onlyScopeMatches builds the -only scope matcher: a step runs iff its
// `scope:` intersects the selected set. The diff is ignored (selection is
// the intent) and an unscoped step never matches — see the Only field doc.
func onlyScopeMatches(only []string) func([]string, map[string][]string, []string) (bool, error) {
	want := make(map[string]struct{}, len(only))
	for _, name := range only {
		want[name] = struct{}{}
	}
	return func(stepScopes []string, _ map[string][]string, _ []string) (bool, error) {
		for _, s := range stepScopes {
			if _, ok := want[s]; ok {
				return true, nil
			}
		}
		return false, nil
	}
}

// parseOnlyList splits the -only flag value into scope names, dropping
// empty fields so "web," or " web , core " behave like the clean form.
func parseOnlyList(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		if name := strings.TrimSpace(part); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// validateOnlyScopes rejects a selection naming a scope the pipeline does
// not declare. A typo must fail loudly: silently selecting nothing would
// report a green run that published no image at all.
func validateOnlyScopes(only []string, scopeDefs map[string][]string) error {
	var unknown []string
	for _, name := range only {
		if _, ok := scopeDefs[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	declared := make([]string, 0, len(scopeDefs))
	for name := range scopeDefs {
		declared = append(declared, name)
	}
	sort.Strings(declared)
	return fmt.Errorf("-only: unknown scope %s; declared scopes are: %s",
		strings.Join(unknown, ", "), strings.Join(declared, ", "))
}

// resolvePipelineTag is a thin wrapper around resolveTag that returns
// the facts alongside the tag so the caller can run placeholder
// substitution on step Run / Env without re-querying git.
func resolvePipelineTag(p Pipeline, repoRoot string) (string, tagFacts, error) {
	if p.Tag == nil {
		return "", tagFacts{}, nil
	}
	if p.Tag.Strategy != "" && p.Tag.Template != "" {
		return "", tagFacts{}, &TagConfigError{Reason: "both"}
	}
	if p.Tag.Strategy == "" && p.Tag.Template == "" {
		return "", tagFacts{}, &TagConfigError{Reason: "neither"}
	}
	facts, err := collectTagFacts(repoRoot, defaultGitRunner, nil)
	if err != nil {
		return "", tagFacts{}, err
	}
	var tag string
	if p.Tag.Strategy != "" {
		tag, err = applyStrategy(p.Tag.Strategy, facts)
		if err != nil {
			return "", tagFacts{}, err
		}
	} else {
		tag = substitutePlaceholders(p.Tag.Template, facts.lookup)
	}
	return tag, facts, nil
}

// substituteEnvPlaceholders rewrites every value in env using the tag
// substituter. Returns the input map unchanged (sharing storage) when
// no value contains a `{{`; otherwise returns a fresh map so the
// caller's struct isn't mutated.
func substituteEnvPlaceholders(env map[string]string, publishTag string, facts tagFacts) map[string]string {
	if len(env) == 0 {
		return env
	}
	var out map[string]string
	for k, v := range env {
		if !strings.Contains(v, "{{") {
			continue
		}
		nv := substituteTagPlaceholders(v, publishTag, facts)
		if nv == v {
			continue
		}
		if out == nil {
			out = make(map[string]string, len(env))
			for kk, vv := range env {
				out[kk] = vv
			}
		}
		out[k] = nv
	}
	if out == nil {
		return env
	}
	return out
}

// mergeStringMaps overlays b onto a (b wins). Used by CompilePipeline
// to inject SKLP_PUBLISH_TAG into the pipeline-level Env without
// mutating the caller's map. Mirrors plan.mergeStringMaps; duplicated
// here to keep the runner package import-clean.
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

// makeInputResolver returns a plan.InputResolver that expands globs
// against repoRoot using doublestar. An empty repoRoot disables
// resolution (the compiler keeps the declared globs but leaves
// ResolvedInputs empty — useful for unit tests that don't need a real
// working tree).
func makeInputResolver(repoRoot string) plan.InputResolver {
	if repoRoot == "" {
		return nil
	}
	return func(globs []string) ([]string, error) {
		return spec.ResolveInputGlobs(repoRoot, globs)
	}
}

// resolveSecrets reads each declared secret name from the host
// environment. Empty values are treated as "not provided". Returned
// map suitable for merging into the step environment.
//
// Values are stripped of a matched pair of surrounding quotes, mirroring
// the env_files parser (stripQuotes). Without this, a host-exported secret
// like S3_SECRET_KEY='"abc"' — quotes included — reaches the AWS client as
// `"abc"` and every SigV4 signature is computed over the wrong key
// (SignatureDoesNotMatch). Host env wins over the .env file, so a quoted
// host value would defeat a fix applied only to the file.
//
// A name in optional may be absent: outside CI a registry password variable
// is unused, the push authenticates from `sklp registry login` instead.
func resolveSecrets(names []string, repoRoot string, optional map[string]bool) (map[string]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(names))
	var missing []string
	for _, name := range names {
		v := os.Getenv(name)
		if v == "" && optional[name] {
			continue
		}
		// The passphrase is the one secret that need not be exported: it
		// lives in the OS vault precisely so no shell has to carry it. Every
		// other name still comes from the environment, where the runner and
		// the deploy target put it.
		if v == "" && name == stackconfig.PassphraseEnv {
			if pp, _, err := stackconfig.ResolvePassphrase(repoRoot); err == nil {
				v = pp
			}
		}
		if v == "" {
			missing = append(missing, name)
			continue
		}
		out[name] = stripQuotes(v)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: missing required secrets: %s", ErrInvalidConfig, strings.Join(missing, ", "))
	}
	return out, nil
}

// expandTilde resolves a leading `~/` segment against $HOME. A bare
// `~` (no trailing slash) is left untouched — Docker treats it as a
// literal path, and we don't want to silently rewrite it.
func expandTilde(p string) string {
	if !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	return home + p[1:]
}

// writerFunc adapts a plain function into an io.Writer so RunPipeline
// can route plan.ExecuteCI's single Stdout / Stderr to whichever step
// is currently active without spinning up an additional goroutine.
type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
