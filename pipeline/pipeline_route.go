package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// isPipelineKind peeks at the YAML's `kind:` field without doing the
// full LoadPipeline (which would require an absolute path and resolve
// `extends:`). We only need to know whether to take the new route.
//
// Legacy task files use `kind: task` (or omit it entirely). The new
// schema uses `kind: pipeline`. Anything else falls through to the
// legacy path so an unknown kind doesn't lock the user out.
func isPipelineKind(taskBytes []byte) bool {
	var peek struct {
		Kind string `yaml:"kind"`
	}
	if err := yaml.Unmarshal(taskBytes, &peek); err != nil {
		return false
	}
	return strings.EqualFold(peek.Kind, "pipeline")
}

// runPipelineFromBytes is the bridge between the Run() entry point and
// the deterministic pipeline executor. It re-loads the YAML through
// LoadPipeline so `extends:` is resolved (the raw taskBytes parse done
// earlier only sees the leaf file), then calls RunPipeline.
//
// The returned Report carries a JSON-encoded PipelineResult in RawJSON
// so the spawner / SSE consumer can decode per-step details without a
// schema change to the existing Report type.
func runPipelineFromBytes(
	ctx context.Context,
	cfg RunConfig,
	taskAbs, repoAbs string,
	stdout, stderr io.Writer,
) (Report, error) {
	p, err := LoadPipeline(taskAbs)
	if err != nil {
		return Report{}, fmt.Errorf("load pipeline %s: %w", taskAbs, err)
	}

	out := stdout
	errOut := stderr
	if cfg.JSONOutput {
		// JSON mode: human-readable progress goes to stderr, the final
		// JSON report goes to stdout. Same contract as the legacy
		// runner so consumers (core/ci spawner) keep working.
		out = stderr
	}

	res, runErr := RunPipeline(ctx, p, PipelineRunOptions{
		RepoPath:      repoAbs,
		BaseRef:       cfg.BaseRef,
		Stdout:        out,
		Stderr:        errOut,
		NoCache:       cfg.NoCache,
		Force:         cfg.Force,
		Only:          cfg.Only,
		Resume:        cfg.Resume,
		Jobs:          cfg.Jobs,
		FailFast:      cfg.FailFast,
		NoRemoteCache: cfg.NoRemoteCache,
		Quiet:         cfg.Quiet,
		TailLines:     cfg.TailLines,
		EnableHotkeys: cfg.EnableHotkeys,
		TaskPath:      taskAbs,
	})

	// 439: every compile-time error must surface a clear "CI FAILED"
	// banner (text mode) or a structured compile_error object (JSON
	// mode). Compile-time = the executor returned an error AND no step
	// produced a result, so the pretty reporter's footer/FAILURES path
	// never had a chance to render anything. Without this branch the
	// CLI exits 1 with no output (issue #439). Require violations get
	// the same dispatch but keep their bespoke banner via the typed
	// branch inside printCompileFailure / emitCompileFailureJSON.
	if isCompileFailure(runErr) {
		taskRel := relTaskPath(taskAbs, repoAbs)
		if cfg.JSONOutput {
			emitCompileFailureJSON(stdout, stderr, runErr, taskRel)
		} else {
			printCompileFailure(stderr, runErr, taskRel)
		}
		// Keep RawJSON non-empty so Main()'s exit-code path still
		// returns 1 via reportExitCode (pass_fail=fail).
		raw, _ := json.Marshal(struct {
			Kind  string `json:"kind"`
			Pass  string `json:"pass_fail"`
			Error string `json:"error"`
		}{Kind: "pipeline", Pass: "fail", Error: runErr.Error()})
		return Report{RawJSON: string(raw), Kind: "pipeline", PassFail: "fail"}, runErr
	}

	// Surface runtime errors to stderr so JSON-mode consumers (sklp CLI,
	// K8sJobSpawner) see what went wrong instead of inferring it from a
	// suspiciously empty report. Without this, an installTools or
	// resolveSecrets failure returns PipelineResult{} (steps==nil) and
	// the JSON encoder happily prints "steps":null as if it were a
	// successful zero-step run — see issue #350.
	if runErr != nil && cfg.JSONOutput {
		fmt.Fprintf(stderr, "[sklp] pipeline error: %v\n", runErr)
	}

	// Text mode: pre-loop bail-outs (env_files, secrets, registry, compile)
	// return before any StepStarted is emitted, so the reporter only ever
	// printed the `sklp pipeline · N step(s)` header. Without a visible
	// failure line the user sees a blank exit-1 — git push hooks just say
	// "CI failed" with nothing to act on. Print the underlying error so
	// the operator can see what broke. The per-step failure path is
	// unaffected: when res.Steps is non-empty, the executor's reporter
	// has already rendered the FAILED banner + FAILURES section.
	if runErr != nil && !cfg.JSONOutput && len(res.Steps) == 0 {
		fmt.Fprintf(stderr, "\nCI FAILED — %v\n", runErr)
	}

	// Force pass_fail to "fail" whenever runErr is non-nil but the
	// executor returned before populating it (early-exit on secrets /
	// tools / config errors). The per-step loop already sets "fail" on
	// step exit, so this branch only catches the pre-loop bail-outs.
	passFail := res.PassFail
	if runErr != nil && passFail == "" {
		passFail = "fail"
	}

	errStr := ""
	if runErr != nil {
		errStr = runErr.Error()
	}

	raw, jerr := json.Marshal(struct {
		Kind  string       `json:"kind"`
		Steps []StepResult `json:"steps"`
		Pass  string       `json:"pass_fail"`
		Error string       `json:"error,omitempty"`
	}{
		Kind:  "pipeline",
		Steps: res.Steps,
		Pass:  passFail,
		Error: errStr,
	})
	if jerr != nil {
		// Encoding our own struct is supposed to be infallible; fail
		// loudly if it ever isn't, with the run error preserved for
		// debugging.
		return Report{}, fmt.Errorf("encode pipeline report: %w (run err: %v)", jerr, runErr)
	}

	if cfg.JSONOutput {
		emitJSONReport(stdout, stderr, raw)
	}

	rep := Report{
		RawJSON:  string(raw),
		Kind:     "pipeline",
		PassFail: passFail,
	}
	return rep, runErr
}

// relTaskPath returns taskAbs relative to repoAbs when possible, falling
// back to the absolute path. Used purely for the require: banner so the
// user sees ".sklp/tasks/test.yaml" rather than a deep absolute path.
func relTaskPath(taskAbs, repoAbs string) string {
	if taskAbs == "" {
		return ""
	}
	if rel, err := filepath.Rel(repoAbs, taskAbs); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return taskAbs
}

// printRequireViolationBanner renders the human-readable refusal block
// to w (stderr in production). Format is fixed so users learn it and
// CI log scrapers can match against it. The body shape mirrors the
// issue #422 spec — first line summarises the failure, optional diff
// stanza lists the offending paths, then a one-liner pointing back at
// the source declaration and a remediation hint.
func printRequireViolationBanner(w io.Writer, err *RequireViolationError, taskPath string) {
	if err == nil || w == nil {
		return
	}
	fmt.Fprintln(w, err.Message)
	if len(err.PorcelainLines) > 0 {
		fmt.Fprintln(w)
		for _, line := range err.PorcelainLines {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}
	fmt.Fprintln(w)
	switch err.Rule {
	case RequireRuleBranches:
		if taskPath != "" {
			fmt.Fprintf(w, "This pipeline declares require.branches: %s in %s.\n", err.Want, taskPath)
		} else {
			fmt.Fprintf(w, "This pipeline declares require.branches: %s.\n", err.Want)
		}
	case RequireRuleCleanTree:
		if taskPath != "" {
			fmt.Fprintf(w, "This pipeline declares require.clean_tree: true in %s.\n", taskPath)
		} else {
			fmt.Fprintln(w, "This pipeline declares require.clean_tree: true.")
		}
	default:
		if taskPath != "" {
			fmt.Fprintf(w, "Declared in %s.\n", taskPath)
		}
	}
	if err.Fix != "" {
		// Capitalise the first rune of Fix for a sentence-cased line
		// without forcing every caller to remember the convention.
		fix := err.Fix
		if r := []rune(fix); len(r) > 0 {
			r[0] = []rune(strings.ToUpper(string(r[0])))[0]
			fix = string(r)
		}
		if !strings.HasSuffix(fix, ".") {
			fix += "."
		}
		fmt.Fprintln(w, fix)
	}
}

// emitRequireViolationJSON writes the structured refusal payload in
// -output json mode. Shape matches the rest of the runner's JSON
// output: a single object on stdout, sentinel-wrapped on stderr so
// single-stream backends (K8sJobSpawner) can still recover it.
func emitRequireViolationJSON(stdout, stderr io.Writer, err *RequireViolationError, taskPath string) {
	if err == nil {
		return
	}
	payload := struct {
		Kind     string            `json:"kind"`
		Error    string            `json:"error"`
		PassFail string            `json:"pass_fail"`
		Details  map[string]string `json:"details"`
	}{
		Kind:     "require_violation",
		Error:    err.Message,
		PassFail: "fail",
		Details: map[string]string{
			"rule": err.Rule,
			"fix":  err.Fix,
		},
	}
	if err.Current != "" {
		payload.Details["current"] = err.Current
	}
	if err.Want != "" {
		payload.Details["want"] = err.Want
	}
	if taskPath != "" {
		payload.Details["task"] = taskPath
	}
	if len(err.PorcelainLines) > 0 {
		payload.Details["status"] = strings.Join(err.PorcelainLines, "\n")
	}
	body, _ := json.Marshal(payload)
	emitJSONReport(stdout, stderr, body)
}
