// sklp is a real-world validation binary: hand it a .sklp/pipelines/*.yaml and a
// target repository, and it asks an LLM agent to interpret and execute the
// contract using the three reusable tools (bash_allowlisted, read_file,
// git_diff).
//
// In --dry-run mode (default) the allowlist excludes any state-changing
// command (docker push, scw push, git push, make docker-publish-*, etc.) —
// the agent can inspect the repo, resolve impacted services, plan the run,
// but cannot mutate anything.
//
// Usage:
//
//	MISTRAL_API_KEY=... go run ./cmd/sklp \
//	  -task ~/code/projects/skalpai/.sklp/pipelines/publish.yaml \
//	  -repo ~/code/projects/skalpai \
//	  -dry-run
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/lalternativefabrique/sklp/internal/limarelay"
	"github.com/lalternativefabrique/sklp/pipeline/spawn"
)

// Main is the flag-parsing CLI entry point used by sklp and the legacy
// sklp CLI. It is a thin wrapper over ParseArgsAndRun: parse os.Args,
// map any error to a non-zero exit. All real work lives in Run; the
// CLI-only paths live in ParseArgsAndRun.
func Main() {
	rep, err := ParseArgsAndRun(context.Background(), os.Args[1:])
	if err != nil {
		log.Fatalf("run: %v", err)
	}
	_ = rep
}

// ParseArgsAndRun is the programmatic CLI entrypoint: it parses argv (the same
// flags Main accepts), handles the CLI-only paths (OCI spawn, -resolve-only,
// self-clone, provider→APIKey, -output), and calls Run. sklp run calls this
// instead of mutating os.Args. Library callers (core/ci) should call Run with a
// RunConfig directly — this function is for argv-driven invocation only.
//
// It returns the final Report and an error. CLI-only early-exit paths (OCI
// spawn, -resolve-only) return a Report (possibly zero) and nil on success.
func ParseArgsAndRun(ctx context.Context, args []string) (Report, error) {
	return ParseArgsAndRunWithOutput(ctx, args, os.Stdout, os.Stderr)
}

// ParseArgsAndRunWithOutput is ParseArgsAndRun with the process streams
// injected. The node run API needs it: it serves a pipeline over HTTP and must
// capture the output rather than let it reach the server's own stdout — while
// still going through THIS function, because the decision to sandbox a run
// (space: false opt-out, require: evaluation, the non-Linux relay) lives here
// and calling Run directly silently skips it.
//
// stdout / stderr default to the process streams when nil.
func ParseArgsAndRunWithOutput(ctx context.Context, args []string, stdout, stderr io.Writer) (Report, error) {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	fs := flag.NewFlagSet("sklp run", flag.ContinueOnError)
	taskPath := fs.String("task", "", "path to .sklp/pipelines/*.yaml")
	repo := fs.String("repo", "", "path to the target repository root")
	// Self-clone mode (used by the K8sJobSpawner): when -repo points at
	// an empty / non-existent directory and -repo-url is provided, the
	// runner clones the repo itself before executing the pipeline. This
	// keeps the DockerSpawner's bind-mount path working unchanged
	// (existing repos are detected and reused) while letting the K8s
	// backend ship a Job with no pre-populated workspace.
	repoURL := fs.String("repo-url", "", "if set and -repo is empty, the runner clones repo-url into -repo before running")
	headRef := fs.String("head-ref", "", "git ref to check out when self-cloning (default: remote HEAD)")
	model := fs.String("model", envOr("SKLP_MODEL", "gpt-oss-120b"), "model id (provider-specific). Defaults to $SKLP_MODEL.")
	provider := fs.String("provider", envOr("SKLP_PROVIDER", "ovh"), "agent provider kind: mistral, deepseek, anthropic, openai, openai-compat, ovh, scaleway. Defaults to $SKLP_PROVIDER.")
	baseURL := fs.String("base-url", envOr("SKLP_BASE_URL", "https://oai.endpoints.kepler.ai.cloud.ovh.net/v1"), "base URL for openai-compat / ovh / scaleway providers. Defaults to $SKLP_BASE_URL.")
	dryRun := fs.Bool("dry-run", true, "refuse state-changing commands (recommended for first runs)")
	output := fs.String("output", "text", "output format: text (verbose, human) or json (final report only on stdout)")
	resolveOnly := fs.Bool("resolve-only", false, "print the pre-resolved plan (impacted services + changed paths) and exit; no LLM call")
	baseRef := fs.String("base-ref", "", "git ref to use as the base for the impacted-services diff. Defaults to HEAD~1.")
	force := fs.Bool("force", false, "ignore the scope/impact filter: every step (kind: pipeline) or service (kind: task, legacy) is treated as in-scope regardless of the git diff. Orthogonal to -no-cache: content-addressed cache hits still serve from disk; combine with -no-cache for a real rebuild.")
	only := fs.String("only", "", "comma-separated list of scopes to run: only the steps whose `scope:` names one of them execute, regardless of the git diff. Steps with no scope never match. An unknown scope is an error listing the declared ones. Combine with -no-cache to force a real rebuild.")
	noPull := fs.Bool("no-pull", false, "skip the automatic `git pull --ff-only` that runs before the impact diff.")
	noCache := fs.Bool("no-cache", false, "bypass the per-step content-addressed cache: every CI step runs even if a previous identical run is on disk, and nothing is written (engine 2.3).")
	noRemoteCache := fs.Bool("no-remote-cache", false, "disable remote-cache lookup and push for this run, regardless of whether SKLP_REMOTE_CACHE_URL is set (engine 2.8).")
	jobs := fs.Int("jobs", 0, "maximum number of CI steps to run in parallel. 0 = the run's CPU budget (SKLP_CPUS, else cgroup cpu.max, else runtime.NumCPU()); 1 reproduces the pre-2.5 sequential behaviour (engine 2.5).")
	failFast := fs.Bool("fail-fast", false, "on the first step failure, cancel every in-flight step instead of letting independent siblings finish (engine 2.5).")
	resume := fs.Bool("resume", false, "short-circuit steps that were `passed` or `cached` in the previous run; restart execution at the first `failed` / `skipped`. Incompatible with --no-cache (engine 2.7).")
	quiet := fs.Bool("quiet", false, "suppress live stdout/stderr streaming under the spinner for kind: pipeline runs; only the final per-step status is shown (engine 2.9).")
	tailLines := fs.Int("tail-lines", 0, "override the live-block per-step tail size. 0 = auto (min(10, terminal_height/4), floor 3). Pressing +/- during a run also adjusts this. (#444)")
	noHotkeys := fs.Bool("no-hotkeys", false, "disable the interactive +/-/f/q hotkeys that grow/shrink/flush the live tail at runtime. Hotkeys are auto-disabled when stdin is not a TTY. (#444)")
	runnerImage := fs.String("runner-image", envOr("SKLP_CI_RUNNER_IMAGE", spawn.DefaultRunnerImage), "OCI image the Space runs. Defaults to $SKLP_CI_RUNNER_IMAGE, then sklp-runner-base:latest.")
	// Secrets the pipeline declares (GITHUB_TOKEN, ...) are surfaced to the
	// Space via this env file. Defaults to <repo>/.env when that file exists
	// — the same file the dev stack reads — so a laptop run just works.
	// Empty disables it.
	envFile := fs.String("env-file", "", "path to a KEY=value env file passed to the Space run (default: <repo>/.env if present).")
	if err := fs.Parse(args); err != nil {
		return Report{}, fmt.Errorf("parse args: %w", err)
	}

	// The Space is the only execution backend. When we are already INSIDE a
	// managed runner container (the Space runtime, or a K8s Pod) — signalled
	// by SKLP_RUNNER_MODE — the inner `sklp run` must run the pipeline
	// directly in-process instead of spawning a Space. Without this, the
	// inner run would re-enter spawnEmbedded and rebuild the runner rootfs a
	// second time, from inside the container's userns. That nested
	// `unshare --map-users=1:100000:65535` fails (EPERM writing uid_map): the
	// container's userns only owns 65536 subids, it cannot delegate the host
	// subuid range one more level down. The container IS the sandbox, so the
	// pipeline runs here directly.
	spawnSpace := !alreadyInSandbox()

	if *taskPath == "" || *repo == "" {
		return Report{}, fmt.Errorf("both -task and -repo are required")
	}

	if *resume && *noCache {
		return Report{}, fmt.Errorf("cannot combine --resume and --no-cache: resume relies on the per-step cache to replay prior passes")
	}

	// Self-clone: if -repo-url is set, materialize the repo into -repo
	// before doing anything else. Skipped when -repo already contains a
	// git checkout (developer running sklp on their laptop).
	if *repoURL != "" {
		if err := ensureRepoCloned(*repo, *repoURL, *headRef, *baseRef); err != nil {
			return Report{}, fmt.Errorf("self-clone: %w", err)
		}
	}

	// -resolve-only: short-circuit before any LLM call. We do this here
	// (in the CLI shell, not in Run) because Run requires an API key
	// and we want -resolve-only to work without one.
	if *resolveOnly {
		repoAbs, err := filepath.Abs(*repo)
		if err != nil {
			return Report{}, fmt.Errorf("repo abs: %w", err)
		}
		taskAbs := *taskPath
		if !filepath.IsAbs(taskAbs) {
			taskAbs = filepath.Join(repoAbs, taskAbs)
		}
		taskBytes, err := os.ReadFile(taskAbs)
		if err != nil {
			return Report{}, fmt.Errorf("read task %s: %w", taskAbs, err)
		}
		effectiveBase := *baseRef
		if effectiveBase == "" {
			effectiveBase = "HEAD~1"
		}
		plan, err := resolve(taskBytes, repoAbs, effectiveBase, "HEAD")
		if err != nil {
			return Report{}, fmt.Errorf("resolve plan: %w", err)
		}
		if *force {
			applyForce(plan, taskBytes)
		}
		// -only is a narrowing selection: it wins over -force's widening.
		if sel := parseOnlyList(*only); len(sel) > 0 {
			if err := validateOnlyScopes(sel, scopeDefsFromTask(taskBytes)); err != nil {
				return Report{}, err
			}
			applyOnly(plan, taskBytes, sel)
		}
		fmt.Print(plan.renderForPrompt())
		return Report{}, nil
	}

	// Resolve API key from the conventional env var for this provider.
	apiKey := ""
	switch *provider {
	case "anthropic":
		apiKey = os.Getenv("ANTHROPIC_API_KEY")
	case "openai", "openai-compat":
		apiKey = os.Getenv("OPENAI_API_KEY")
	case "ovh":
		if k := os.Getenv("OVH_KEY"); k != "" {
			apiKey = k
		} else {
			apiKey = os.Getenv("OVH_API_KEY")
		}
	case "scaleway":
		apiKey = os.Getenv("SCW_SECRET_KEY")
	case "deepseek":
		apiKey = os.Getenv("DEEPSEEK_API_KEY")
	default: // mistral
		apiKey = os.Getenv("MISTRAL_API_KEY")
	}

	jsonMode := *output == "json"
	if jsonMode {
		log.SetOutput(os.Stderr)
	}

	// space: false — pipeline opts out of the Space sandbox, runs on host.
	// -task is repo-relative (e.g. .sklp/pipelines/publish.yaml): resolving it
	// against the process cwd instead of -repo silently no-ops this check for
	// any caller whose cwd isn't the repo root — the node run API in
	// particular, whose cwd is the service's WorkingDirectory, not the
	// self-cloned repo passed via -repo. LoadPipeline then fails, err != nil,
	// and the pipeline falls through into the Space sandbox it explicitly
	// opted out of.
	if spawnSpace {
		taskAbs := *taskPath
		if !filepath.IsAbs(taskAbs) {
			if repoAbs, err := filepath.Abs(*repo); err == nil {
				taskAbs = filepath.Join(repoAbs, taskAbs)
			}
		}
		if p, err := LoadPipeline(taskAbs); err == nil && p.Space != nil && !*p.Space {
			spawnSpace = false
		}
	}

	// require: gate — hoisted ABOVE both execution paths. The Space path
	// builds a rootfs / runs apt before it ever reaches RunPipeline's own
	// EvaluateRequire, so a refused run (e.g. publish off main) would
	// otherwise touch Docker-equivalent work before being rejected. We
	// evaluate here, against the run's actual repo root, so NO
	// space/rootfs/build happens on a require failure. The returned error
	// wraps ErrRequireViolation, keeping flow end's skip classification
	// (classifyRunErr) working. RunPipeline's own gate stays as the
	// authoritative in-process check (no-op re-eval here is cheap: one `git`
	// call, and only on the Space path that bypasses it).
	if spawnSpace {
		if err := evaluateRequireForTask(ctx, *taskPath, *repo); err != nil {
			return Report{}, err
		}
	}

	// Off Linux the Space sandbox cannot run locally: relay this exact
	// invocation into the Lima VM instead (provisioned on demand). The
	// guest re-runs `sklp run …` on Linux, where the normal spawn below
	// applies; RelaySelf only returns on failure.
	if spawnSpace && runtime.GOOS != "linux" {
		repoAbs, err := filepath.Abs(*repo)
		if err != nil {
			return Report{}, fmt.Errorf("repo abs: %w", err)
		}
		if err := limarelay.RelaySelf(repoAbs); err != nil {
			return Report{}, fmt.Errorf("oci run: this pipeline runs in the Space sandbox — %v", err)
		}
	}

	// Space engine: hand the run off to a container and exit. We do this
	// here, after API-key resolution, so the spawned runner inherits the
	// same provider credentials the in-process path would use. The Space
	// runs `sklp run <task>` with SKLP_RUNNER_MODE set, so it executes
	// in-process — no recursive spawn.
	if spawnSpace {
		repoAbs, err := filepath.Abs(*repo)
		if err != nil {
			return Report{}, fmt.Errorf("repo abs: %w", err)
		}
		// Forward the execution flags the operator passed on the host so
		// the in-container run behaves identically. Provider / model /
		// output are passed via env or fixed argv above, so they are NOT
		// repeated here. The inner run detects SKLP_RUNNER_MODE and runs
		// directly in-process, so no recursive spawn happens.
		//
		// -no-pull is forced on: the repo is bind-mounted from the host,
		// so the operator means "run my local state". A `git pull` in the
		// container would try to advance the working tree to the remote
		// (and fails anyway — no ssh in the image). The host -no-pull flag
		// is therefore redundant in Space mode but harmless.
		extra := []string{"-no-pull"}
		if *force {
			extra = append(extra, "-force")
		}
		if *noCache {
			extra = append(extra, "-no-cache")
		}
		if *noRemoteCache {
			extra = append(extra, "-no-remote-cache")
		}
		if *failFast {
			extra = append(extra, "-fail-fast")
		}
		if *resume {
			extra = append(extra, "-resume")
		}
		if *quiet {
			extra = append(extra, "-quiet")
		}
		if *baseRef != "" {
			extra = append(extra, "-base-ref", *baseRef)
		}
		if *only != "" {
			extra = append(extra, "-only", *only)
		}
		if *jobs != 0 {
			extra = append(extra, "-jobs", fmt.Sprintf("%d", *jobs))
		}

		// Resolve the env file: explicit -env-file wins; otherwise fall
		// back to <repo>/.env when it exists (the dev-stack convention),
		// so a laptop run surfaces GITHUB_TOKEN & friends without a flag.
		resolvedEnvFile := *envFile
		if resolvedEnvFile == "" {
			if def := filepath.Join(repoAbs, ".env"); fileExists(def) {
				resolvedEnvFile = def
			}
		}

		// The multi-container Space runtime runs host-side, so we parse the
		// pipeline HERE and map its services to spawn.Service. Parsing
		// host-side also fails fast on a malformed pipeline, before any
		// image pull.
		taskAbs := *taskPath
		if !filepath.IsAbs(taskAbs) {
			taskAbs = filepath.Join(repoAbs, taskAbs)
		}
		p, err := LoadPipeline(taskAbs)
		if err != nil {
			return Report{}, fmt.Errorf("space: load pipeline %s: %w", taskAbs, err)
		}
		ociServices := pipelineServicesForEmbedded(p)

		// Bin "space" routes OCISpawner.Spawn to spawnEmbedded (daemonless runc).
		sp := &spawn.OCISpawner{Image: *runnerImage, Bin: "space"}
		report, err := sp.Spawn(ctx, spawn.Config{
			RepoPath: repoAbs,
			TaskName: taskName(*taskPath),
			Provider: spawn.Provider{
				Kind:    *provider,
				Model:   *model,
				BaseURL: *baseURL,
				APIKey:  apiKey,
			},
			ExtraArgs: extra,
			EnvFile:   resolvedEnvFile,
			LiveOut:   stderr,
			Services:  ociServices,
		})
		if report != "" && jsonMode {
			fmt.Fprintln(stdout, report)
		}
		if err != nil {
			if report != "" {
				// Surface the report's own exit code (may be non-1) to the
				// caller without killing the process here.
				return decodeReport(report, "", false, nil), &exitCodeError{code: reportExitCode(report)}
			}
			return Report{}, fmt.Errorf("oci run: %w", err)
		}
		return decodeReport(report, "", false, nil), nil
	}

	cfg := RunConfig{
		TaskPath:      *taskPath,
		RepoPath:      *repo,
		Provider:      *provider,
		Model:         *model,
		BaseURL:       *baseURL,
		APIKey:        apiKey,
		DryRun:        *dryRun,
		BaseRef:       *baseRef,
		Force:         *force,
		Only:          parseOnlyList(*only),
		NoPull:        *noPull,
		NoCache:       *noCache,
		NoRemoteCache: *noRemoteCache,
		Resume:        *resume,
		Jobs:          *jobs,
		FailFast:      *failFast,
		Quiet:         *quiet,
		TailLines:     *tailLines,
		EnableHotkeys: !*noHotkeys,
		Stdout:        stdout,
		Stderr:        stderr,
		JSONOutput:    jsonMode,
	}

	rep, err := Run(ctx, cfg)
	if err != nil {
		// Non-zero exit code from the agent's report comes back as an
		// error too; surface the report's exit code to the caller.
		if rep.RawJSON != "" {
			return rep, &exitCodeError{code: reportExitCode(rep.RawJSON)}
		}
		return rep, err
	}
	return rep, nil
}

// evaluateRequireForTask loads the pipeline at taskPath (resolved against
// repo when relative) and runs its require: block against the repo root.
// Used to gate the container path before any Space/rootfs/build work; the
// in-process path is gated by RunPipeline directly. Returns nil when the
// pipeline declares no require: block.
func evaluateRequireForTask(ctx context.Context, taskPath, repo string) error {
	repoAbs, err := filepath.Abs(repo)
	if err != nil {
		return fmt.Errorf("repo abs: %w", err)
	}
	taskAbs := taskPath
	if !filepath.IsAbs(taskAbs) {
		taskAbs = filepath.Join(repoAbs, taskAbs)
	}
	p, err := LoadPipeline(taskAbs)
	if err != nil {
		return fmt.Errorf("load pipeline %s: %w", taskAbs, err)
	}
	if err := EvaluateTrigger(ctx, p.On, repoAbs); err != nil {
		return err
	}
	return EvaluateRequire(ctx, p.Require, repoAbs)
}

// exitCodeError carries the process exit code a CLI caller should use,
// preserving report-driven non-1 codes through the (Report, error) return.
type exitCodeError struct{ code int }

func (e *exitCodeError) Error() string { return fmt.Sprintf("run failed (exit %d)", e.code) }
func (e *exitCodeError) ExitCode() int { return e.code }

// ExitCode returns the process exit code an error implies: the carried
// code for an *exitCodeError, else 1 for any non-nil error, else 0.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ec *exitCodeError
	if errors.As(err, &ec) {
		return ec.code
	}
	return 1
}

// reportExitCode inspects the report and returns 0 on success, 1 on failure.
// Both report shapes are handled: CI (pass_fail) and publish (preflight_results).
// taskName derives the logical task name the container runs from the
// -task path. `.sklp/pipelines/ci.yaml` → "ci". An empty path falls back to
// the spawner's own default ("ci").
func taskName(taskPath string) string {
	if taskPath == "" {
		return ""
	}
	base := filepath.Base(taskPath)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// alreadyInSandbox reports whether this process is already executing inside a
// managed runner container (the Space runtime or a K8s Pod), signalled by
// SKLP_RUNNER_MODE. When true, the pipeline must run directly in-process: the
// container IS the sandbox, and spawning a Space here would recurse into a
// nested userns that fails with EPERM.
func alreadyInSandbox() bool {
	mode := os.Getenv(RunnerModeEnv)
	return mode == RunnerModeEmbedded || mode == RunnerModeK8s
}

func reportExitCode(jsonReport string) int {
	var r struct {
		PassFail         string `json:"pass_fail"`
		PreflightResults []struct {
			Status string `json:"status"`
		} `json:"preflight_results"`
	}
	if err := json.Unmarshal([]byte(jsonReport), &r); err != nil {
		return 1
	}
	if r.PassFail == "fail" {
		return 1
	}
	for _, p := range r.PreflightResults {
		if p.Status == "failed" {
			return 1
		}
	}
	return 0
}

// detectKind reads the `name:` field of a .sklp/pipelines/*.yaml so the
// caller can reject a legacy contract by kind. Falls back to "unknown".
var nameRe = regexp.MustCompile(`(?m)^name:\s*(\w+)\s*$`)

func detectKind(yaml string) string {
	if m := nameRe.FindStringSubmatch(yaml); len(m) == 2 {
		return strings.ToLower(m[1])
	}
	return "unknown"
}

// fileExists reports whether path is an existing regular file.
func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}
