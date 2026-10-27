package plan

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// RemoteCacheOutput pairs one CAS digest with its repo-relative path
// in a remote step result. Mirrors CIStepCacheOutput on the wire.
type RemoteCacheOutput struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

// RemoteStepResult is the value the executor exchanges with a remote
// backend. Modeled identically to the local CIStepCacheResult so the
// transport layer is a thin pass-through.
type RemoteStepResult struct {
	Name       string              `json:"name"`
	ExitCode   int                 `json:"exit_code"`
	Status     string              `json:"status"`
	DurationMS int64               `json:"duration_ms"`
	Outputs    []RemoteCacheOutput `json:"outputs,omitempty"`
	Stdout     []byte              `json:"-"`
	Stderr     []byte              `json:"-"`
}

// ErrRemoteCacheMiss tells the executor a step is not present in the
// remote backend. Any other error is treated as a transient transport
// failure (logged, then fall back to local execution).
var ErrRemoteCacheMiss = errors.New("remote cache miss")

// RemoteCache is the transport-agnostic contract the executor expects
// from a remote backend. internal/remotecache implements it on top of
// S3-compatible storage; tests inject a fake.
type RemoteCache interface {
	GetResult(ctx context.Context, stepKey string) (*RemoteStepResult, error)
	PutResult(ctx context.Context, stepKey string, res *RemoteStepResult) error
	GetCAS(ctx context.Context, sha string) (io.ReadCloser, error)
	PutCAS(ctx context.Context, sha string, r io.Reader, size int64) error
	HasCAS(ctx context.Context, sha string) (bool, error)
}

// CachedNote is the per-step note surfaced via OnStepEnd when the
// executor served a result from the content-addressed cache. Stable
// for tests and the pretty reporter.
const CachedNote = "served from cache"

// CIExecConfig configures CI plan execution. Kept separate from
// ExecConfig because the CI executor uses a different shape — full
// `sh -c` snippets, per-step env overlays, tool resolution callback —
// that would clutter the publish-focused ExecConfig.
type CIExecConfig struct {
	// Plan is the compiled CI plan. Kind must be KindCI and Plan.CI
	// must be non-nil; ExecuteCI returns an error otherwise.
	Plan *Plan

	// Repo is the absolute repo root. Steps run with Dir set here.
	Repo string

	// ToolsCacheDir is the per-host toolchain cache root (where skt /
	// native installers extract tool binaries). Granted read-only under
	// FS confinement so resolved tools stay reachable. Empty disables
	// the extra grant (tool bin dirs are still granted via ToolDirs).
	ToolsCacheDir string

	// BaseEnv is the host environment to filter through the sandbox
	// allowlist (PATH, HOME, USER, TZ, LANG, LC_ALL). Typically the
	// caller passes os.Environ(). Anything outside the allowlist is
	// dropped; pipeline/step env and opted-in secrets are layered on
	// top by the sandbox.
	BaseEnv []string

	// Secrets is the resolved secret name -> value map. A step only
	// sees the subset it declares via CIStep.Secrets. Materialisation
	// at <SecretDir>/<NAME> is gated on the same declaration.
	Secrets map[string]string

	// SecretDir is where opted-in secrets are written as files. Empty
	// makes the executor pick a writable location (/run/secrets when
	// available, a per-run temp dir otherwise).
	SecretDir string

	// Stdout / Stderr receive every line produced by every step. The
	// CI executor does no line-wrapping itself; the caller plugs in
	// the same reporter writers it used to plug into pipeline_exec.
	Stdout io.Writer
	Stderr io.Writer

	// MakeStepWriters, when non-nil, supplies dedicated per-step
	// (stdout, stderr) sinks. It is the live-reporter contract: the
	// scheduler bypasses its own per-step buffering and routes the
	// shell stdout/stderr straight to the writers the runner built
	// for that step name. Line coherence then falls on the writer
	// implementation (the runner uses a line-splitting writer).
	//
	// When MakeStepWriters is nil the scheduler falls back to its
	// internal buffering against Stdout / Stderr (engine-2.5 path).
	MakeStepWriters func(name string) (stdout, stderr io.Writer)

	// ResolveTool, when non-nil, is invoked for each tool name a step
	// declares to obtain the bin directory to prepend to PATH. Returns
	// an error to fail the step.
	//
	// Kept as a callback (instead of importing the toolchain package
	// directly) so internal/plan stays free of the runner's host /
	// native / skt resolution ladder.
	ResolveTool func(ctx context.Context, name string) (binDir string, err error)

	// OnStepStart / OnStepEnd let the caller render live progress.
	// Safe to leave nil.
	OnStepStart func(ev CIStepEvent)
	OnStepEnd   func(ev CIStepEvent)

	// PerStepTimeout caps each individual step. Zero = no timeout.
	PerStepTimeout time.Duration

	// Cache, when non-nil, is consulted before spawning each step and
	// populated after a successful run. Nil disables both read and
	// write (e.g. when the caller wants raw execution).
	Cache *StepCache

	// PlanKey is the content-addressed key of the compiled CI plan as
	// a whole — typically ComputeCIPlanKey(Plan). It namespaces every
	// step-key beneath it so two independently-evolving pipelines in
	// the same repo never share a per-step cache namespace.
	//
	// Ignored when Cache is nil. When Cache is set, an empty PlanKey
	// is an error.
	PlanKey string

	// CAS, when non-nil, is the content-addressed store the executor
	// uses to capture declared outputs after a successful step and
	// restore them on a cache hit. Disabled (and ignored) when
	// Cache is nil or NoCache is true.
	CAS *CAS

	// NoCache, when true, forces a full re-execution: the executor
	// neither reads from nor writes to Cache. Mirrors `sklp run
	// --no-cache`.
	NoCache bool

	// Jobs caps the number of steps that may run concurrently. Zero
	// (the default) means the run's CPU budget (SKLP_CPUS, else the
	// cgroup cpu.max, else runtime.NumCPU()). A value of 1 forces the
	// pre-2.5 sequential behaviour: steps run in declared topo order
	// and their stdout/stderr stream through to the caller's writers
	// live (no per-step buffering).
	Jobs int

	// FailFast, when true, cancels every in-flight step on the first
	// failure. Off by default: siblings without a transitive dependency
	// on the failing step keep running so the user gets the maximum
	// signal out of a broken pipeline.
	FailFast bool

	// LiveReporter, when true, declares that OnStepStart / OnStepEnd
	// are safe to call concurrently and the caller wants step-start
	// events to fire the moment a worker picks the step up — not when
	// its buffered output flushes. With Jobs > 1 + LiveReporter, the
	// scheduler issues OnStepStart immediately (so a live TUI can show
	// every active step's spinner) and serialises only the buffered
	// log dump + OnStepEnd, which is enough to keep per-step logs
	// coherent. With Jobs == 1 or LiveReporter == false the pre-2.5
	// behaviour is preserved verbatim.
	LiveReporter bool

	// Resume, when true, makes ExecuteCI consult the previous run's
	// last.json (engine 2.7). Steps whose previous status was `passed`
	// or `cached` are short-circuited via the existing per-step cache;
	// the first step with any other prior status falls through to
	// normal execution and downstream steps run normally regardless of
	// their previous status. Resume is a no-op when no record exists
	// for the current plan key. Incompatible with NoCache (callers
	// must reject the combination upstream).
	Resume bool

	// resumeEligible is the per-step "may short-circuit via cache" set
	// computed once by ExecuteCI when Resume is true and the previous
	// run's last.json was loadable. Internal — not part of the public
	// configuration surface.
	resumeEligible map[string]bool

	// Remote, when non-nil, is consulted on a local cache miss and
	// pushed to after a successful exec. Uploads are best-effort and
	// non-blocking: a failure logs to Stderr and does not fail the
	// step. ExecuteCI drains in-flight uploads at the end of the run
	// (subject to RemotePushTimeout).
	Remote RemoteCache

	// RemotePushTimeout caps how long ExecuteCI waits for in-flight
	// remote uploads after the last step finishes. Zero defaults to
	// 30s. A hung S3 connection must not deadlock the run.
	RemotePushTimeout time.Duration

	// OnRemoteEvent, when non-nil, is invoked for observable remote
	// cache transitions ("hit", "miss", "push_ok", "push_fail").
	// Internal — used by tests; production code reads through Stderr.
	OnRemoteEvent func(stepName, event string)

	// OnRemotePushDone, when non-nil, is invoked once per upload
	// completion (success or failure). Used by tests to await async
	// uploads deterministically.
	OnRemotePushDone func()

	// pushWG tracks the in-flight remote upload goroutines so
	// ExecuteCI can drain them at the end of the run.
	pushWG *sync.WaitGroup

	// PublishExec, when non-nil, replaces the default sandboxed
	// exec used by the `kind: publish` orchestrator. Test seam
	// only — production callers leave it nil so each phase runs through
	// runSandboxed (issue #411).
	PublishExec publishExec
}

// CIStepEvent describes a CI step transition. Mirrors StepEvent
// (publish path) but is dedicated to the CI shape so the publish
// reporter and the CI reporter can keep their fields separate.
type CIStepEvent struct {
	Name     string
	Status   StepStatus
	Duration time.Duration
	Note     string
	// SkipReason is set when the step was StatusSkippedCompile or
	// skipped because a previous step failed.
	SkipReason string
}

// CIReport is the deterministic CI executor output. Mirrors today's
// runner.PipelineResult so the runner package can decode the same
// shape it used to produce directly.
type CIReport struct {
	Steps     []CIStepResult `json:"steps"`
	PassFail  string         `json:"pass_fail"` // "pass" | "fail"
	RuntimeMs int64          `json:"runtime_ms,omitempty"`
}

// CIStepResult is the per-step outcome.
type CIStepResult struct {
	Name     string `json:"name"`
	Status   string `json:"status"` // "passed" | "failed" | "skipped"
	ExitCode int    `json:"exit_code,omitempty"`
	// DurationMS must NOT be omitempty: a step that ran for <1ms has a
	// legitimate duration of 0, and omitting the field then makes the
	// JSON report non-deterministic (present when ≥1ms, absent when 0).
	// TestScheduler_DeterministicReport flaked exactly on this — two
	// identical runs differed only by a 0ms step's missing field.
	DurationMS int64  `json:"duration_ms"`
	SkipReason string `json:"skip_reason,omitempty"`
}

// ExecuteCI runs every included step of a compiled CI plan. Steps with
// no DAG edge between them execute concurrently up to cfg.Jobs (default
// the CPU budget, see cpuBudget). A non-zero exit on a step marks its transitive
// descendants as skipped; siblings continue unless cfg.FailFast is set.
//
// The executor is deliberately small: every runtime conditional that
// pipeline_exec.go used to host (scope gate, env merging, if:) has
// been resolved at compile, so the worker body reads as "for each step:
// resolve tools -> run sh -c -> record result".
func ExecuteCI(ctx context.Context, cfg CIExecConfig) (*CIReport, error) {
	if cfg.Plan == nil {
		return nil, errors.New("ExecuteCI: plan is nil")
	}
	if cfg.Plan.Kind != KindCI || cfg.Plan.CI == nil {
		return nil, fmt.Errorf("ExecuteCI: plan kind=%q ci=%v not a CI plan", cfg.Plan.Kind, cfg.Plan.CI != nil)
	}
	if cfg.Repo == "" {
		return nil, errors.New("ExecuteCI: repo path is empty")
	}
	if cfg.Cache != nil && !cfg.NoCache && cfg.PlanKey == "" {
		return nil, errors.New("ExecuteCI: Cache is set but PlanKey is empty")
	}
	if cfg.Resume && cfg.NoCache {
		return nil, errors.New("ExecuteCI: Resume and NoCache are mutually exclusive")
	}

	rep := &CIReport{Steps: make([]CIStepResult, 0, len(cfg.Plan.CI.Steps))}
	t0 := time.Now()
	defer func() { rep.RuntimeMs = time.Since(t0).Milliseconds() }()

	// Resume eligibility: walk steps in declared topo order, mark each
	// step whose previous status was `passed` or `cached` as eligible
	// for the cache short-circuit. Stop at the first non-success status:
	// every subsequent step runs normally regardless of its prior state.
	// Missing or unreadable last.json is a silent miss — resume becomes
	// a no-op without preventing the run.
	if cfg.Resume && cfg.PlanKey != "" {
		if rec, err := LoadLastRun(cfg.Repo, cfg.PlanKey); err == nil {
			prev := make(map[string]string, len(rec.Steps))
			for _, s := range rec.Steps {
				prev[s.Name] = s.Status
			}
			eligible := make(map[string]bool, len(cfg.Plan.CI.Steps))
			for _, s := range cfg.Plan.CI.Steps {
				st, ok := prev[s.Name]
				if !ok {
					break
				}
				if st != "passed" && st != "cached" {
					break
				}
				eligible[s.Name] = true
			}
			cfg.resumeEligible = eligible
		}
	}

	secretDir := cfg.SecretDir
	if secretDir == "" {
		// Safety net for runs killed by SIGKILL before their deferred cleanup:
		// sweep orphaned per-run secret dirs under our scoped cache root.
		reapOrphanSecretDirs()
		dir, cleanup, err := chooseSecretDir()
		if err != nil {
			return nil, fmt.Errorf("ExecuteCI: %w", err)
		}
		defer cleanup()
		secretDir = dir
	}

	// Remote-cache upload tracker. Lives at the ExecuteCI level so
	// every in-flight goroutine launched by runStep can be drained
	// before we return, regardless of the scheduler's parallelism.
	if cfg.Remote != nil {
		wg := &sync.WaitGroup{}
		cfg.pushWG = wg
		defer drainRemotePushes(cfg, wg)
	}

	results, err := runScheduler(ctx, cfg, secretDir)
	if err != nil {
		return nil, err
	}

	failed := false
	for _, r := range results {
		if r.Status == "failed" {
			failed = true
		}
		rep.Steps = append(rep.Steps, r)
	}
	if failed {
		rep.PassFail = "fail"
	} else {
		rep.PassFail = "pass"
	}

	// Persist the per-step status array as the new last-run record so a
	// follow-up `sklp run --resume` can short-circuit prior passes. We
	// only write when caching is enabled and a plan key is known: with
	// NoCache or a missing key, resume cannot work anyway, so the file
	// would just be dead weight. Write errors are non-fatal.
	if cfg.Cache != nil && !cfg.NoCache && cfg.PlanKey != "" {
		_ = SaveLastRun(cfg.Repo, cfg.PlanKey, rep.Steps)
	}

	return rep, nil
}

// stepOutcome carries the result of runStep plus whatever extra state
// the scheduler needs to render the lifecycle events atomically.
type stepOutcome struct {
	Result CIStepResult
	Event  CIStepEvent
	Failed bool
	// Cached is true when the result came from the on-disk cache. The
	// scheduler uses this to decide whether OnStepStart should be fired
	// (cached steps still fire start+end so reporters can distinguish a
	// "cached" badge from a fresh pass).
	Cached bool
	// CapturedOutputs records the (path → digest) manifest captured into
	// the CAS for a fresh pass. Empty for cache hits, failures, or steps
	// that declared no outputs. Exposed for tests / observability.
	CapturedOutputs []CIStepCacheOutput
}

// runStep executes the per-step body: cache lookup, tool resolution,
// sandboxed spawn, cache store. It is called by the scheduler's worker
// goroutines. It does NOT fire OnStepStart / OnStepEnd — the scheduler
// emits those under its flush mutex so lifecycle events stay coherent
// with the buffered log flush.
func runStep(ctx context.Context, cfg CIExecConfig, step CIStep, stepKey, secretDir string, stdoutW, stderrW io.Writer) stepOutcome {
	res := CIStepResult{Name: step.Name}

	// Whether the step cache applies at all, decided once: a run-wide
	// bypass (-no-cache), or a step that declared `cache: false` because
	// its effect lands outside the repo. Gates BOTH lookup and store —
	// caching an opted-out step would let a LATER run replay it.
	useCache := cfg.Cache != nil && !cfg.NoCache && !step.NoCache

	// Cache lookup BEFORE we even announce the step started — on a
	// hit we want to report `cached` directly, not show a spinner
	// for a step that will not spawn anything.
	if useCache {
		cres, soutBytes, serrBytes, lerr := cfg.Cache.LookupStep(cfg.PlanKey, stepKey)
		if lerr != nil && cfg.Resume && cfg.resumeEligible[step.Name] && stderrW != nil {
			// Resume promised this step was a previous pass, but the
			// per-step cache entry is gone (eviction race, manual
			// cleanup, or a transitive key change). Surface the gap
			// and fall through to a normal run rather than failing.
			fmt.Fprintf(stderrW, "[sklp] resume: cache entry missing for step %q, running normally\n", step.Name)
		}
		if lerr == nil {
			if rerr := Replay(soutBytes, serrBytes, stdoutW, stderrW); rerr != nil {
				res.Status = "failed"
				res.ExitCode = -1
				ev := CIStepEvent{Name: step.Name, Status: StatusFailed, Note: fmt.Sprintf("replay cached step: %v", rerr)}
				return stepOutcome{Result: res, Event: ev, Failed: true, Cached: true}
			}
			// 2.4: output restore — restore every declared output
			// recorded in the cache entry back into the working tree
			// so a downstream step (or a follow-up sklp run) sees the
			// same files a fresh execution would have produced.
			if cfg.CAS != nil {
				if rerr := restoreOutputs(cfg.CAS, cfg.Repo, cres.Outputs); rerr != nil {
					res.Status = "failed"
					res.ExitCode = -1
					ev := CIStepEvent{Name: step.Name, Status: StatusFailed, Note: fmt.Sprintf("restore outputs: %v", rerr)}
					return stepOutcome{Result: res, Event: ev, Failed: true, Cached: true}
				}
			}
			dur := time.Duration(cres.DurationMS) * time.Millisecond
			res.Status = "cached"
			res.ExitCode = cres.ExitCode
			res.DurationMS = cres.DurationMS
			ev := CIStepEvent{Name: step.Name, Status: StatusCached, Duration: dur, Note: CachedNote}
			return stepOutcome{Result: res, Event: ev, Cached: true}
		}

		// Local miss: consult the remote backend before spawning a
		// process. On hit we hydrate the local cache + CAS + working
		// tree so subsequent runs are local-fast. Transport failures
		// fall through silently to normal execution.
		if cfg.Remote != nil && cfg.CAS != nil {
			if outcome, ok := tryRemoteHit(ctx, cfg, step, stepKey, stdoutW, stderrW); ok {
				return outcome
			}
		}
	}

	// Resolve tools (host lookup / install). A failure here is a
	// step failure with the offending tool name in the note.
	var toolDirs []string
	if len(step.Tools) > 0 && cfg.ResolveTool != nil {
		var toolErr error
		for _, name := range step.Tools {
			dir, err := cfg.ResolveTool(ctx, name)
			if err != nil {
				toolErr = fmt.Errorf("tool %s: %w", name, err)
				break
			}
			toolDirs = append(toolDirs, dir)
		}
		if toolErr != nil {
			res.Status = "failed"
			res.ExitCode = -1
			ev := CIStepEvent{Name: step.Name, Status: StatusFailed, Note: toolErr.Error()}
			return stepOutcome{Result: res, Event: ev, Failed: true}
		}
	}

	// Tee the streams through buffers so we can persist exactly what
	// the user saw on cache hit. The capture is unconditional only
	// when caching is enabled; without a cache the buffers stay
	// nil and we write straight through.
	var stdoutBuf, stderrBuf *bytes.Buffer
	spawnStdout := stdoutW
	spawnStderr := stderrW
	if useCache {
		stdoutBuf = &bytes.Buffer{}
		stderrBuf = &bytes.Buffer{}
		spawnStdout = teeWriter(stdoutW, stdoutBuf)
		spawnStderr = teeWriter(stderrW, stderrBuf)
	}

	stepStart := time.Now()
	var code int
	var err error
	if step.StepKind == KindCIStepPublish {
		// The publishExec seam (cfg.PublishExec) is non-nil only in
		// tests; production paths fall back to sandboxedExec.
		execFn := cfg.PublishExec
		if execFn == nil {
			execFn = sandboxedExec
		}
		code, err = runPublish(ctx, cfg, step, secretDir, spawnStdout, spawnStderr, execFn)
	} else {
		// Step env is used verbatim: services are addressed by name on the
		// bridge (e.g. postgres:5432), no port-map merge or interpolation.
		stepEnv := step.Env
		goMod, goCache, goBin := resolveGoCacheDirs(stepEnv, cfg.BaseEnv)
		// Only node/pnpm steps need the corepack cache; granting this $HOME
		// dir to a Go-only step would needlessly widen its sandbox.
		var nodeCache string
		if stepUsesTool(step, "node", "pnpm", "yarn", "npm") {
			nodeCache = resolveNodeCacheDir(stepEnv, cfg.BaseEnv)
		}
		gitConfig := resolveGitConfig(stepEnv, cfg.BaseEnv)
		gitIgnoreGlobal := resolveGitIgnoreGlobal(stepEnv, cfg.BaseEnv)
		sklpUserConfig := resolveSklpUserConfig(stepEnv, cfg.BaseEnv)
		dockerCfgDir := resolveDockerConfigDir(stepEnv, cfg.BaseEnv)
		spec := sandboxSpec{
			Repo:            cfg.Repo,
			Cwd:             step.Cwd,
			Cmdline:         step.Run,
			StepEnv:         stepEnv,
			ToolDirs:        toolDirs,
			HostEnv:         cfg.BaseEnv,
			Secrets:         step.Secrets,
			AllSecret:       cfg.Secrets,
			Network:         step.Network,
			Filesystem:      step.Filesystem,
			CacheDirs:       cacheDirs(cfg.ToolsCacheDir),
			GoModCache:      goMod,
			GoCache:         goCache,
			GoBin:           goBin,
			GitConfig:       gitConfig,
			GitIgnoreGlobal: gitIgnoreGlobal,
			SklpUserConfig:  sklpUserConfig,
			DockerConfigDir: dockerCfgDir,
			NodeCacheDir:    nodeCache,
			SecretDir:       secretDir,
			Stdout:          spawnStdout,
			Stderr:          spawnStderr,
			Timeout:         cfg.PerStepTimeout,
		}
		code, err = runSandboxed(ctx, spec)
	}
	dur := time.Since(stepStart)
	res.DurationMS = dur.Milliseconds()
	res.ExitCode = code

	status := StatusPassed
	note := ""
	failed := false
	if err != nil || code != 0 {
		failed = true
		res.Status = "failed"
		status = StatusFailed
		if err != nil {
			note = err.Error()
		} else {
			note = fmt.Sprintf("exit %d", code)
		}
	} else {
		res.Status = "passed"
	}

	// Persist successful runs to the cache. Failed runs are NOT
	// cached: replaying a failure would mask a transient infra
	// error (network blip, OOM) as a deterministic verdict.
	var capturedOutputs []CIStepCacheOutput
	if useCache && res.Status == "passed" {
		// 2.4: output capture — copy each declared output into the
		// CAS and record the (path → digest) pairs alongside the
		// step result so a future cache hit can restore them.
		if cfg.CAS != nil && len(step.Outputs) > 0 {
			outs, oerr := captureOutputs(cfg.CAS, cfg.Repo, step.Outputs)
			if oerr != nil {
				note = fmt.Sprintf("output capture failed: %v", oerr)
			}
			capturedOutputs = outs
		}
		cacheRes := CIStepCacheResult{
			Name:       step.Name,
			ExitCode:   code,
			Status:     res.Status,
			DurationMS: res.DurationMS,
			Outputs:    capturedOutputs,
		}
		var sb, eb []byte
		if stdoutBuf != nil {
			sb = stdoutBuf.Bytes()
		}
		if stderrBuf != nil {
			eb = stderrBuf.Bytes()
		}
		if serr := cfg.Cache.StoreStep(cfg.PlanKey, stepKey, cacheRes, sb, eb); serr != nil {
			// Cache write failure is non-fatal: surface it via the
			// step note so the user sees it, but the step itself
			// still counts as passed.
			note = fmt.Sprintf("cache store failed: %v", serr)
		}
		// Best-effort remote push. Launched async because a slow S3
		// connection must never gate the next step; a failure logs to
		// Stderr and does NOT fail the step.
		if cfg.Remote != nil && cfg.pushWG != nil {
			cfg.pushWG.Add(1)
			go pushToRemote(ctx, cfg, step.Name, stepKey, cacheRes, sb, eb)
		}
	}

	ev := CIStepEvent{Name: step.Name, Status: status, Duration: dur, Note: note}
	return stepOutcome{Result: res, Event: ev, Failed: failed, CapturedOutputs: capturedOutputs}
}

// captureOutputs walks each declared output path under repo and copies
// the regular files it finds into cas, returning a (path → digest)
// manifest. Directories expand recursively. A missing path is silently
// skipped — a step may legitimately produce a subset of its declared
// outputs (e.g. an `inputs:` filter narrowed the work). Paths are
// stored repo-relative with forward slashes for portability.
func captureOutputs(cas *CAS, repo string, outputs []string) ([]CIStepCacheOutput, error) {
	if cas == nil || len(outputs) == 0 {
		return nil, nil
	}
	var captured []CIStepCacheOutput
	for _, rel := range outputs {
		abs := filepath.Join(repo, rel)
		info, err := os.Lstat(abs)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return captured, fmt.Errorf("stat output %s: %w", rel, err)
		}
		// Skip symlinks: the CAS stores bytes, not link metadata.
		// Following a symlink would also risk escaping the repo root.
		if info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if info.IsDir() {
			werr := filepath.WalkDir(abs, func(path string, d fs.DirEntry, werr error) error {
				if werr != nil {
					return werr
				}
				if d.IsDir() {
					return nil
				}
				fi, ferr := d.Info()
				if ferr != nil {
					return ferr
				}
				if !fi.Mode().IsRegular() {
					return nil
				}
				digest, perr := cas.PutFile(path)
				if perr != nil {
					return perr
				}
				inner, rerr := filepath.Rel(repo, path)
				if rerr != nil {
					return rerr
				}
				captured = append(captured, CIStepCacheOutput{
					Path:   filepath.ToSlash(inner),
					Digest: digest,
				})
				return nil
			})
			if werr != nil {
				return captured, fmt.Errorf("walk output %s: %w", rel, werr)
			}
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		digest, perr := cas.PutFile(abs)
		if perr != nil {
			return captured, fmt.Errorf("capture output %s: %w", rel, perr)
		}
		captured = append(captured, CIStepCacheOutput{
			Path:   filepath.ToSlash(rel),
			Digest: digest,
		})
	}
	return captured, nil
}

// restoreOutputs writes each (path → digest) entry from a cache
// manifest back into the working tree, materialising parent
// directories as needed. A digest missing from the CAS is a hard
// error: the cache entry promised an output and the store cannot
// deliver, which would silently mislead a downstream step.
func restoreOutputs(cas *CAS, repo string, outputs []CIStepCacheOutput) error {
	if cas == nil || len(outputs) == 0 {
		return nil
	}
	for _, o := range outputs {
		// Defence-in-depth path check: the path was validated at
		// compile time, but a tampered cache file could re-introduce
		// an escape — refuse to write outside the repo.
		clean := filepath.Clean(o.Path)
		if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
			return fmt.Errorf("restore output: refusing escape path %q", o.Path)
		}
		dst := filepath.Join(repo, clean)
		if err := cas.GetTo(o.Digest, dst); err != nil {
			return fmt.Errorf("restore %s: %w", o.Path, err)
		}
	}
	return nil
}

// resolveGoCacheDirs returns the GOMODCACHE, GOCACHE and GOBIN paths the
// confined sandbox must grant for a Go build to work, following Go's own
// resolution order. It is pure (env in, paths out) so the policy stays
// testable. Step env wins over host env, matching composeEnv layering.
//
//	GOMODCACHE: $GOMODCACHE | $GOPATH/pkg | $HOME/go/pkg (whole pkg root: the
//	            grant must also cover the sibling pkg/sumdb the toolchain writes)
//	GOCACHE:    $GOCACHE    | $HOME/.cache/go-build
//	GOBIN:      $GOBIN      | $GOPATH/bin     | $HOME/go/bin
//
// Empty results when no path can be derived (no HOME, no GOPATH); the
// policy then simply adds no grant for that root.
func resolveGoCacheDirs(stepEnv map[string]string, hostEnv []string) (goModCache, goCache, goBin string) {
	get := func(key string) string { return envLookup(key, stepEnv, hostEnv) }

	home := get("HOME")
	gopath := get("GOPATH")
	if gopath == "" && home != "" {
		gopath = filepath.Join(home, "go")
	}

	goModCache = get("GOMODCACHE")
	if goModCache == "" && gopath != "" {
		// Grant the whole $GOPATH/pkg root, not just pkg/mod: the Go
		// toolchain also writes the checksum db under $GOPATH/pkg/sumdb
		// (a sibling of mod), which a pkg/mod-only grant would deny.
		goModCache = filepath.Join(gopath, "pkg")
	}

	goCache = get("GOCACHE")
	if goCache == "" && home != "" {
		goCache = filepath.Join(home, ".cache", "go-build")
	}

	goBin = get("GOBIN")
	if goBin == "" && gopath != "" {
		goBin = filepath.Join(gopath, "bin")
	}
	return goModCache, goCache, goBin
}

// stepUsesTool reports whether the step declares any of the named tools in its
// `tools:` list. Used to scope $HOME cache grants to the steps that need them.
func stepUsesTool(step CIStep, names ...string) bool {
	for _, t := range step.Tools {
		for _, n := range names {
			if t == n {
				return true
			}
		}
	}
	return false
}

// resolveNodeCacheDir returns the corepack download-cache dir the confined
// sandbox must grant read-write for a node/pnpm step, following corepack's own
// resolution order. corepack lazily fetches the pinned pnpm/yarn here and reads
// it back on every later run, so the grant must be writable and host-shared.
//
//	COREPACK_HOME | $XDG_CACHE_HOME/node/corepack | $HOME/.cache/node/corepack
//
// Empty when no path can be derived (no HOME, no XDG_CACHE_HOME); the policy
// then adds no grant — a step that never triggers a corepack download is fine,
// and one that does fails with the same EACCES it would have without sklp.
func resolveNodeCacheDir(stepEnv map[string]string, hostEnv []string) string {
	if v := envLookup("COREPACK_HOME", stepEnv, hostEnv); v != "" {
		return v
	}
	if xdg := envLookup("XDG_CACHE_HOME", stepEnv, hostEnv); xdg != "" {
		return filepath.Join(xdg, "node", "corepack")
	}
	if home := envLookup("HOME", stepEnv, hostEnv); home != "" {
		return filepath.Join(home, ".cache", "node", "corepack")
	}
	return ""
}

// resolveGitConfig returns the user's ~/.gitconfig path (or "" when HOME is
// unknown). git reads it for user.name, url rewrites, and the credential
// helper; under the confined sandbox $HOME is denied, so the policy grants
// just this one file read-only. GIT_CONFIG_GLOBAL overrides when set.
//
// The path is symlink-resolved: dotfile managers (stow, chezmoi) symlink
// ~/.gitconfig to a file under e.g. ~/code/dotfiles. Landlock follows the
// link at open time, so granting the link (not its target) leaves git with
// "Permission denied" on the real file. Grant the resolved target instead.
func resolveGitConfig(stepEnv map[string]string, hostEnv []string) string {
	if v := envLookup("GIT_CONFIG_GLOBAL", stepEnv, hostEnv); v != "" {
		return evalSymlinks(v)
	}
	if home := envLookup("HOME", stepEnv, hostEnv); home != "" {
		return evalSymlinks(filepath.Join(home, ".gitconfig"))
	}
	return ""
}

// resolveGitIgnoreGlobal returns git's global excludes file — $XDG_CONFIG_HOME
// /git/ignore, or ~/.config/git/ignore when XDG_CONFIG_HOME is unset — the
// file git consults on every `add`/`status` regardless of core.excludesFile.
// Returns "" when HOME/XDG_CONFIG_HOME is unknown or the file does not exist,
// so an unconfigured host grants nothing extra.
func resolveGitIgnoreGlobal(stepEnv map[string]string, hostEnv []string) string {
	return resolveXDGConfigFile(stepEnv, hostEnv, "git", "ignore")
}

// resolveSklpUserConfig returns the sklp CLI's own config file — see
// internal/config.Path(): $XDG_CONFIG_HOME/sklp/config.yaml, or
// ~/.config/sklp/config.yaml when XDG_CONFIG_HOME is unset. stackconfig.Load
// reads it to resolve the active deploy context. Returns "" when HOME/
// XDG_CONFIG_HOME is unknown or the file does not exist.
func resolveSklpUserConfig(stepEnv map[string]string, hostEnv []string) string {
	return resolveXDGConfigFile(stepEnv, hostEnv, "sklp", "config.yaml")
}

// resolveXDGConfigFile resolves $XDG_CONFIG_HOME/<elem...>, falling back to
// ~/.config/<elem...> when XDG_CONFIG_HOME is unset. Returns "" when HOME/
// XDG_CONFIG_HOME is unknown or the resolved file does not exist, so an
// unconfigured host grants nothing extra.
func resolveXDGConfigFile(stepEnv map[string]string, hostEnv []string, elem ...string) string {
	base := envLookup("XDG_CONFIG_HOME", stepEnv, hostEnv)
	if base == "" {
		home := envLookup("HOME", stepEnv, hostEnv)
		if home == "" {
			return ""
		}
		base = filepath.Join(home, ".config")
	}
	path := filepath.Join(append([]string{base}, elem...)...)
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return evalSymlinks(path)
}

// resolveDockerConfigDir returns the ~/.docker config dir (or "" when HOME
// is unknown). docker buildx locks and mutates it (buildx/.lock, instances/,
// refs/, current); under the confined sandbox $HOME is denied, so the policy
// grants this dir read-write for Docker steps. DOCKER_CONFIG overrides when
// set. Symlink-resolved for the same reason as resolveGitConfig (a symlinked
// ~/.docker would otherwise be granted by link, not target).
func resolveDockerConfigDir(stepEnv map[string]string, hostEnv []string) string {
	if v := envLookup("DOCKER_CONFIG", stepEnv, hostEnv); v != "" {
		return evalSymlinks(v)
	}
	if home := envLookup("HOME", stepEnv, hostEnv); home != "" {
		return evalSymlinks(filepath.Join(home, ".docker"))
	}
	return ""
}

// evalSymlinks returns the symlink-resolved path, or the original path
// unchanged when it cannot be resolved (missing target, broken link). The
// fallback is safe: Landlock grants use IgnoreIfMissing, so a non-existent
// path simply adds no rule.
func evalSymlinks(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

// envLookup reads a variable from the step env (preferred) then the host
// env (KEY=VALUE slice). Empty values are treated as unset.
func envLookup(key string, stepEnv map[string]string, hostEnv []string) string {
	if v, ok := stepEnv[key]; ok && v != "" {
		return v
	}
	prefix := key + "="
	for _, kv := range hostEnv {
		if strings.HasPrefix(kv, prefix) {
			if v := kv[len(prefix):]; v != "" {
				return v
			}
		}
	}
	return ""
}

// overlayEnv layers a string-keyed map onto a `KEY=VALUE` slice.
// Later keys win. Mirrors runner.mergeEnv but is kept local so
// internal/plan does not need to reach into the runner package.
func overlayEnv(base []string, overlay map[string]string) []string {
	if len(overlay) == 0 {
		return base
	}
	idx := make(map[string]int, len(base))
	out := make([]string, len(base))
	copy(out, base)
	for i, kv := range out {
		if eq := strings.IndexByte(kv, '='); eq > 0 {
			idx[kv[:eq]] = i
		}
	}
	for k, v := range overlay {
		kv := k + "=" + v
		if i, ok := idx[k]; ok {
			out[i] = kv
			continue
		}
		idx[k] = len(out)
		out = append(out, kv)
	}
	return out
}

// teeWriter returns a writer that mirrors every write to both `to`
// and `also`. A nil `to` makes the tee write only to `also`, so the
// executor can capture output even when the caller did not supply a
// stream (e.g. unit tests that only care about the cached bytes).
func teeWriter(to, also io.Writer) io.Writer {
	if to == nil {
		return also
	}
	return io.MultiWriter(to, also)
}

// prependEnvPath prepends one or more directories to the PATH entry
// of env, preserving every other variable in place.
func prependEnvPath(env []string, dirs []string) []string {
	if len(dirs) == 0 {
		return env
	}
	const key = "PATH="
	prefix := strings.Join(dirs, ":")
	for i, kv := range env {
		if strings.HasPrefix(kv, key) {
			out := make([]string, len(env))
			copy(out, env)
			out[i] = key + prefix + ":" + kv[len(key):]
			return out
		}
	}
	return append(append([]string(nil), env...), key+prefix)
}
