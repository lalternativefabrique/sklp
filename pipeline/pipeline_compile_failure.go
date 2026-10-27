// Package runner — generic compile-failure surface for `sklp run`.
//
// Issue #439: every error returned by RunPipeline BEFORE the reporter
// begins rendering steps must produce a clear "CI FAILED" banner on
// stderr (text mode) or a structured `compile_error` object on stdout
// (JSON mode). Anything less leaves the user staring at exit 1 with no
// clue what broke — the bug shape that bit us three times in 24h.
//
// New typed errors plug into this surface by adding one entry to the
// hint table below; no new printer is introduced per concern.
package pipeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/lalternativefabrique/sklp/internal/plan"
)

// isCompileFailure reports whether err originated in RunPipeline's
// pre-loop compile / resolve phase — the phase that runs BEFORE any
// step is started or rendered by the reporter. Used by the dispatch
// path to decide between the generic CI FAILED banner (compile) and
// the executor's own footer (runtime).
//
// The shape is structural: any known compile-time typed error matches,
// as does anything wrapping the compile-time sentinels
// (ErrInvalidConfig, ErrRequireViolation) or the plan.CICompileError
// emitted by CompilePipeline.
func isCompileFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrRequireViolation) ||
		errors.Is(err, ErrInvalidConfig) {
		return true
	}
	var (
		rve *RequireViolationError
		reg *MissingCredentialError
		bk  *BuildKitSecretEnvMissingError
		tag *TagConfigError
		ce  *plan.CICompileError
	)
	switch {
	case errors.As(err, &rve),
		errors.As(err, &reg),
		errors.As(err, &bk),
		errors.As(err, &tag),
		errors.As(err, &ce):
		return true
	}
	return false
}

// printCompileFailure renders the human-readable "CI FAILED" banner +
// the error message + an optional typed-error-specific hint to w
// (stderr in production). taskPath is the repo-relative path to the
// pipeline file when known; empty otherwise.
//
// Special case: RequireViolationError keeps its bespoke rich format
// (PR #424) so the regression test pinning the porcelain stanza still
// passes. Every other compile error funnels through the generic banner.
func printCompileFailure(w io.Writer, err error, taskPath string) {
	if err == nil || w == nil {
		return
	}
	var rve *RequireViolationError
	if errors.As(err, &rve) {
		printRequireViolationBanner(w, rve, taskPath)
		return
	}
	fmt.Fprintln(w, "CI FAILED")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "FAILURES")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  pipeline could not compile")
	fmt.Fprintln(w)
	for _, line := range strings.Split(strings.TrimRight(compileFailureMessage(err), "\n"), "\n") {
		fmt.Fprintf(w, "      %s\n", line)
	}
	if hint := compileFailureHint(err); hint != "" {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "      hint: %s\n", hint)
	}
}

// emitCompileFailureJSON writes the structured compile-error payload in
// -output json mode. Shape mirrors the require_violation object so JSON
// consumers can switch on `kind` and find `error` + `details` either
// way. Require violations keep their dedicated kind for back-compat.
func emitCompileFailureJSON(stdout, stderr io.Writer, err error, taskPath string) {
	if err == nil {
		return
	}
	var rve *RequireViolationError
	if errors.As(err, &rve) {
		emitRequireViolationJSON(stdout, stderr, rve, taskPath)
		return
	}
	details := compileFailureDetails(err)
	if taskPath != "" {
		if details == nil {
			details = map[string]string{}
		}
		details["task"] = taskPath
	}
	if hint := compileFailureHint(err); hint != "" {
		if details == nil {
			details = map[string]string{}
		}
		details["hint"] = hint
	}
	payload := struct {
		Kind     string            `json:"kind"`
		Error    string            `json:"error"`
		PassFail string            `json:"pass_fail"`
		Details  map[string]string `json:"details,omitempty"`
	}{
		Kind:     "compile_error",
		Error:    compileFailureMessage(err),
		PassFail: "fail",
		Details:  details,
	}
	body, _ := json.Marshal(payload)
	emitJSONReport(stdout, stderr, body)
}

// compileFailureMessage returns the most specific human-readable error
// message available: when err wraps a typed compile-time error, prefer
// the typed message (it strips the "ci compile: " prefix added by
// plan.CICompileError) so the banner reads cleanly.
func compileFailureMessage(err error) string {
	var regErr *MissingCredentialError
	if errors.As(err, &regErr) {
		return regErr.Error()
	}
	var bkErr *BuildKitSecretEnvMissingError
	if errors.As(err, &bkErr) {
		return bkErr.Error()
	}
	var tagErr *TagConfigError
	if errors.As(err, &tagErr) {
		return tagErr.Error()
	}
	return err.Error()
}

// compileFailureHint returns a remediation hint for the typed errors
// the compile path can produce. Unknown error types yield "" so the
// banner shows just the bare error message — which is still leagues
// better than today's silent exit.
func compileFailureHint(err error) string {
	var regErr *MissingCredentialError
	if errors.As(err, &regErr) {
		return "run the `sklp registry login` above, then re-run"
	}
	var ciErr *CIRegistryError
	if errors.As(err, &ciErr) {
		return "add the host to the pipeline `registries:` block and a credential for its images in the stack's Registry tab, then re-run"
	}
	var bkErr *BuildKitSecretEnvMissingError
	if errors.As(err, &bkErr) {
		return fmt.Sprintf(
			"set %s in your shell or in .env / .env.<pipeline>, then re-run",
			bkErr.EnvVar,
		)
	}
	var tagErr *TagConfigError
	if errors.As(err, &tagErr) {
		return "edit `tag:` to set exactly one of `strategy` or `template`"
	}
	var ce *plan.CICompileError
	if errors.As(err, &ce) {
		// Generic compile error: no specific hint. The reason string
		// already carries the actionable detail.
		return ""
	}
	return ""
}

// compileFailureDetails surfaces typed-error fields into the JSON
// payload so machine consumers can branch without re-parsing the
// human message. Returns nil for untyped errors.
func compileFailureDetails(err error) map[string]string {
	out := map[string]string{}
	var regErr *MissingCredentialError
	if errors.As(err, &regErr) {
		out["category"] = "registry_credential_missing"
		out["hosts"] = strings.Join(regErr.Hosts, ",")
		return out
	}
	var ciErr *CIRegistryError
	if errors.As(err, &ciErr) {
		out["category"] = "ci_registry_credential_missing"
		hosts := append([]string{}, ciErr.Undeclared...)
		for h := range ciErr.EmptyVar {
			hosts = append(hosts, h)
		}
		sort.Strings(hosts)
		out["hosts"] = strings.Join(hosts, ",")
		return out
	}
	var bkErr *BuildKitSecretEnvMissingError
	if errors.As(err, &bkErr) {
		out["category"] = "buildkit_secret_env_missing"
		out["step"] = bkErr.Step
		out["secret_id"] = bkErr.SecretID
		out["env_var"] = bkErr.EnvVar
		return out
	}
	var tagErr *TagConfigError
	if errors.As(err, &tagErr) {
		out["category"] = "tag_config"
		out["reason"] = tagErr.Reason
		if tagErr.Strategy != "" {
			out["strategy"] = tagErr.Strategy
		}
		return out
	}
	var ce *plan.CICompileError
	if errors.As(err, &ce) {
		out["category"] = "ci_compile"
		if ce.Step != "" {
			out["step"] = ce.Step
		}
		out["reason"] = ce.Reason
		return out
	}
	return nil
}
