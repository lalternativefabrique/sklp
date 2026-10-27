package plan

import (
	"errors"
	"fmt"
	"strings"
)

// ValidationError lists every problem found by Validate. Callers
// typically log it and fall back to the agent runner; the categorised
// reasons let us surface a precise hint instead of a generic "plan
// failed validation".
type ValidationError struct {
	Issues []ValidationIssue
}

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Issues))
	for i, iss := range e.Issues {
		parts[i] = iss.String()
	}
	return "plan validation failed: " + strings.Join(parts, "; ")
}

// ValidationIssue is one specific problem with a Plan. Code is a
// stable identifier; Path locates the field; Detail is human-readable.
type ValidationIssue struct {
	Code   IssueCode
	Path   string
	Detail string
}

func (i ValidationIssue) String() string {
	return fmt.Sprintf("[%s] %s: %s", i.Code, i.Path, i.Detail)
}

// IssueCode categorises validation failures.
type IssueCode string

const (
	IssueWrongSchemaVersion   IssueCode = "wrong_schema_version"
	IssueMissingKind          IssueCode = "missing_kind"
	IssueEmptyCommand         IssueCode = "empty_command"
	IssueCommandNotAllowed    IssueCode = "command_not_in_allowlist"
	IssueUnknownPlaceholder   IssueCode = "unknown_placeholder"
	IssueShellMetacharacter   IssueCode = "shell_metacharacter"
	IssueUnknownDep           IssueCode = "unknown_dep"
	IssueEmptyServiceSteps    IssueCode = "empty_service_steps"
	IssueDanglingExpansion    IssueCode = "dangling_expansion_target"
	IssueAbortServicesMissing IssueCode = "abort_services_missing_list"
	IssueExpectedMissing      IssueCode = "expected_missing_for_stdout_equals"
)

// ValidateConfig configures Validate. Allowlist is the same []string
// passed to tools.BashAllowlistedConfig.Allowlist — entries are either
// bare programs ("git", "make") or program+subcommand ("git diff").
type ValidateConfig struct {
	Allowlist []string
}

// Validate inspects a Plan and reports every issue at once. Returning
// a multi-error (rather than failing fast) lets us surface "your plan
// has 3 problems" in a single stderr line; the cost is negligible.
//
// A nil error means the Plan is safe to execute. A non-nil error is
// always a *ValidationError; callers can type-assert to inspect the
// list of issues.
func Validate(p *Plan, cfg ValidateConfig) error {
	if p == nil {
		return &ValidationError{Issues: []ValidationIssue{{Code: IssueMissingKind, Path: "(root)", Detail: "plan is nil"}}}
	}

	var issues []ValidationIssue

	if p.SchemaVersion != SchemaVersion {
		issues = append(issues, ValidationIssue{
			Code: IssueWrongSchemaVersion, Path: "schema_version",
			Detail: fmt.Sprintf("got %d, want %d", p.SchemaVersion, SchemaVersion),
		})
	}
	if p.Kind == "" {
		issues = append(issues, ValidationIssue{Code: IssueMissingKind, Path: "kind", Detail: "empty"})
	}

	allow := compileAllowlist(cfg.Allowlist)

	// Preflights.
	for i, pf := range p.Preflights {
		path := fmt.Sprintf("preflights[%d:%s]", i, pf.ID)
		if len(pf.Commands) == 0 {
			issues = append(issues, ValidationIssue{Code: IssueEmptyCommand, Path: path + ".commands", Detail: "no commands"})
		}
		for j, cmd := range pf.Commands {
			issues = append(issues, validateCommand(fmt.Sprintf("%s.commands[%d]", path, j), cmd, allow, false)...)
		}
		if pf.SuccessCheck == SuccessStdoutEquals && pf.OnFail.Expected == "" {
			issues = append(issues, ValidationIssue{
				Code: IssueExpectedMissing, Path: path + ".on_fail.expected",
				Detail: "success_check=stdout_equals requires on_fail.expected",
			})
		}
		if pf.OnFail.Mode == AbortServices && len(pf.OnFail.AbortedServices) == 0 {
			issues = append(issues, ValidationIssue{
				Code: IssueAbortServicesMissing, Path: path + ".on_fail.aborted_services",
				Detail: "mode=services requires a non-empty aborted_services list",
			})
		}
	}

	// Services: build / scan / push.
	knownServices := make(map[string]struct{}, len(p.Services))
	for name := range p.Services {
		knownServices[name] = struct{}{}
	}
	for name, svc := range p.Services {
		path := fmt.Sprintf("services[%s]", name)
		if len(svc.Build) == 0 && len(svc.Push) == 0 {
			issues = append(issues, ValidationIssue{Code: IssueEmptyServiceSteps, Path: path, Detail: "no build and no push"})
		}
		for j, cmd := range svc.Build {
			issues = append(issues, validateCommand(fmt.Sprintf("%s.build[%d]", path, j), cmd, allow, true)...)
		}
		for j, cmd := range svc.Scan {
			issues = append(issues, validateCommand(fmt.Sprintf("%s.scan[%d]", path, j), cmd, allow, true)...)
		}
		for j, cmd := range svc.Push {
			issues = append(issues, validateCommand(fmt.Sprintf("%s.push[%d]", path, j), cmd, allow, true)...)
		}
	}

	// Deps: every dep target must be a known service. (A dep outside
	// the impacted set is fine — the executor's topological sort
	// handles that — but a dep that doesn't exist anywhere is a bug.)
	for svc, deps := range p.Deps {
		for _, d := range deps {
			if _, ok := knownServices[d]; !ok {
				issues = append(issues, ValidationIssue{
					Code: IssueUnknownDep, Path: fmt.Sprintf("deps[%s]", svc),
					Detail: fmt.Sprintf("dep %q is not a declared service", d),
				})
			}
		}
	}

	// Expansion rules: every target must be a known service. The
	// virtual when_service NEVER needs to be in services (it has no
	// build target by definition).
	for i, rule := range p.ExpansionRules {
		for _, target := range rule.Publish {
			if _, ok := knownServices[target]; !ok {
				issues = append(issues, ValidationIssue{
					Code: IssueDanglingExpansion, Path: fmt.Sprintf("expansion_rules[%d]", i),
					Detail: fmt.Sprintf("publish target %q is not a declared service", target),
				})
			}
		}
	}

	if len(issues) == 0 {
		return nil
	}
	return &ValidationError{Issues: issues}
}

// validateCommand splits a command into tokens, checks placeholders,
// shell metachars, and allowlist membership. allowPublishTagAndService
// controls whether <publish_tag> / <service> are permitted in this
// position (false for preflights, true for service steps).
func validateCommand(path, cmd string, allow allowSets, allowPublishTagAndService bool) []ValidationIssue {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return []ValidationIssue{{Code: IssueEmptyCommand, Path: path, Detail: "empty command"}}
	}

	var issues []ValidationIssue

	// Shell metacharacters are rejected at runtime by bash_allowlisted
	// (forbiddenChars in tools/bash.go). Catching them here gives the
	// compiler a fast failure with a precise pointer.
	//
	// We check on the placeholder-free form so the angle brackets of
	// <publish_tag> / <service> don't trigger a false positive — at
	// runtime they will have been substituted to a plain string.
	cmdNoPH := stripPlaceholders(cmd)
	const forbidden = "|&;$`<>\n\r"
	if strings.ContainsAny(cmdNoPH, forbidden) {
		issues = append(issues, ValidationIssue{
			Code: IssueShellMetacharacter, Path: path,
			Detail: "contains a forbidden shell metacharacter (one of |&;$`<>) — executor refuses these at runtime",
		})
	}

	// Placeholders: only <publish_tag> and <service> are recognised.
	// Anything else (e.g. <namespace_id>) means the compiler did not
	// fully substitute the contract, which would surface as a literal
	// "<foo>" reaching the shell at run time.
	for _, ph := range findPlaceholders(cmd) {
		if ph == "<publish_tag>" || ph == "<service>" {
			if !allowPublishTagAndService && ph == "<publish_tag>" {
				issues = append(issues, ValidationIssue{
					Code: IssueUnknownPlaceholder, Path: path,
					Detail: "<publish_tag> is not allowed in preflight commands (they run before the tag is computed)",
				})
			}
			continue
		}
		issues = append(issues, ValidationIssue{
			Code: IssueUnknownPlaceholder, Path: path,
			Detail: fmt.Sprintf("unknown placeholder %s — only <publish_tag> and <service> are recognised", ph),
		})
	}

	// Allowlist: split on whitespace, take program + first arg as the
	// (cmd, sub) tuple bash_allowlisted will check. Placeholders are
	// stripped first because they will not be part of the program name.
	tokens := strings.Fields(stripPlaceholders(cmd))
	if len(tokens) == 0 {
		// Already reported by IssueEmptyCommand above.
		return issues
	}
	program := tokens[0]
	if strings.ContainsAny(program, "/\\") {
		issues = append(issues, ValidationIssue{
			Code: IssueCommandNotAllowed, Path: path,
			Detail: fmt.Sprintf("program %q is a path, not a bare basename — bash_allowlisted rejects this", program),
		})
		return issues
	}
	var first string
	if len(tokens) > 1 {
		first = tokens[1]
	}
	if !allow.contains(program, first) {
		issues = append(issues, ValidationIssue{
			Code: IssueCommandNotAllowed, Path: path,
			Detail: fmt.Sprintf("%q (first arg %q) is not in the allowlist", program, first),
		})
	}
	return issues
}

// findPlaceholders returns every <...> token in s, including the
// angle brackets. Helps surface a clear error pointing at the exact
// unrecognised template.
func findPlaceholders(s string) []string {
	var out []string
	for {
		i := strings.IndexByte(s, '<')
		if i < 0 {
			return out
		}
		j := strings.IndexByte(s[i:], '>')
		if j < 0 {
			return out
		}
		out = append(out, s[i:i+j+1])
		s = s[i+j+1:]
	}
}

// stripPlaceholders replaces every <...> with a single token so
// splitting on whitespace doesn't accidentally pick up a placeholder
// as the program name (e.g. `<publish_tag>` should never become the
// program).
func stripPlaceholders(s string) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(s, '<')
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		j := strings.IndexByte(s[i:], '>')
		if j < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		b.WriteString("X") // placeholder for the substituted value
		s = s[i+j+1:]
	}
}

// allowSets mirrors the (bareCmd, cmdSub) pair compiled inside
// tools/bash.go NewBashAllowlisted. We keep the exact same semantics
// (bare basename → any args; "cmd sub" → only that subcommand).
type allowSets struct {
	bare map[string]struct{}
	sub  map[[2]string]struct{}
}

func compileAllowlist(entries []string) allowSets {
	out := allowSets{
		bare: make(map[string]struct{}),
		sub:  make(map[[2]string]struct{}),
	}
	for _, e := range entries {
		parts := strings.Fields(e)
		switch len(parts) {
		case 0:
			continue
		case 1:
			out.bare[parts[0]] = struct{}{}
		default:
			out.sub[[2]string{parts[0], parts[1]}] = struct{}{}
		}
	}
	return out
}

func (a allowSets) contains(cmd, firstArg string) bool {
	if _, ok := a.bare[cmd]; ok {
		return true
	}
	if firstArg == "" {
		return false
	}
	_, ok := a.sub[[2]string{cmd, firstArg}]
	return ok
}

// AsValidationError unwraps an error into *ValidationError if it is
// one. Convenience for callers that want to inspect issues without a
// type assertion at the call site.
func AsValidationError(err error) (*ValidationError, bool) {
	var v *ValidationError
	if errors.As(err, &v) {
		return v, true
	}
	return nil, false
}
