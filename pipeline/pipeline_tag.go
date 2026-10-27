// Package runner — declarative publish-tag strategy.
//
// A pipeline that opts into Tag (see schema.go) computes ONE tag at
// compile time, exports it as $SKLP_PUBLISH_TAG to every step, and
// substitutes occurrences of `{{ publish_tag }}` in step Run blocks
// and Env values. The substitution is intentionally a minimal
// placeholder pass — not a general template engine — so the user-facing
// surface stays small and audit-friendly.
//
// Strategies (mutually exclusive with Template):
//
//	date-sha → "<date>-<short_sha>"   e.g. 20260516-1856-bf44d9d
//	sha      → "<short_sha>"
//	date     → "<date>"
//
// Template placeholders (used by Template, and also resolved inside
// step Run / Env values via substituteTagPlaceholders):
//
//	{{ short_sha }} — git HEAD, truncated to ShortSHALen
//	{{ full_sha }}  — `git rev-parse HEAD`
//	{{ date }}      — build time, UTC, `YYYYMMDD-HHMM`
//	{{ branch }}    — current branch, kebab-cased (slashes → '-')
//	{{ publish_tag }} — the resolved tag itself (only valid in step
//	                    Run / Env values, not in TagBlock.Template)
//
// `{{ date }}` is when the image is BUILT, not HEAD's committer date, and the
// difference decides whether a release ships. Image Updater picks the highest
// tag alphabetically, so tags must increase monotonically with publish order.
// Committer dates do not: a bot that commits with an earlier timestamp than the
// commit it lands on top of (ArgoCD's `build: automatic update` is the usual
// culprit) mints a tag sorting BELOW what is already deployed, and the rollout
// silently never happens. Build time cannot go backwards between two runs, so
// the later publish always wins. The SHA already carries the provenance.
package pipeline

import (
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// BuildDateLayout is the `{{ date }}` shape: UTC, minute resolution.
//
// UTC, not local time, because the tag has to keep sorting correctly across a
// DST change and across machines in different zones — a local-time tag jumps
// backwards by an hour every autumn, which is exactly the failure this date
// exists to prevent.
//
// Minute resolution, NOT seconds, and this width is load-bearing. ArgoCD Image
// Updater selects with `update-strategy: alphabetical` behind an `allow-tags`
// regexp pinned to `^[0-9]{8}-[0-9]{4}-[0-9a-f]+$`: a six-digit time fails that
// filter outright, and even if the filter were widened the two widths could not
// coexist, because alphabetical comparison is per character, not numeric —
// `20260731-085134` sorts BELOW `20260731-1200`. Widening the time field is
// therefore a coordinated migration of every allow-tags annotation, not a local
// change here.
//
// The cost of minute resolution: two publishes inside the same minute tie on
// the date and fall back to the short SHA, which sorts arbitrarily. That window
// is narrow — a publish takes minutes — and it is not the failure this file
// exists to prevent, which was tags moving backwards by HOURS.
const BuildDateLayout = "20060102-1504"

// ShortSHALen is the fixed width of {{ short_sha }}. 12 is the git-wide
// convention and stays collision-free far past this repo's history.
const ShortSHALen = 12

// clock returns the current time. Injected so tests can pin `{{ date }}`
// instead of asserting against whatever the wall clock says mid-run.
type clock func() time.Time

// Recognised strategy values. Kept as constants so callers (tests,
// future codegen) can refer to them without typos.
const (
	TagStrategyDateSha = "date-sha"
	TagStrategySha     = "sha"
	TagStrategyDate    = "date"
)

// PublishTagEnvVar is the env var name exposed to every step when Tag
// is declared on the pipeline.
const PublishTagEnvVar = "SKLP_PUBLISH_TAG"

// tagPlaceholderRE matches a `{{ name }}` placeholder with optional
// surrounding whitespace. Intentionally restricted to [a-zA-Z_][\w]*
// so a stray `{{ foo bar }}` is left untouched rather than swallowed.
var tagPlaceholderRE = regexp.MustCompile(`\{\{\s*([a-zA-Z_]\w*)\s*\}\}`)

// gitRunner abstracts `git -C <repo> <args>` so tests can inject a
// fake without spawning a real process. Returns trimmed stdout on
// success.
type gitRunner func(repoRoot string, args ...string) (string, error)

// defaultGitRunner shells out to git and returns trimmed stdout. Errors
// include git's stderr so a missing repo / detached HEAD is diagnosable
// from the failure message alone.
func defaultGitRunner(repoRoot string, args ...string) (string, error) {
	full := append([]string{"-C", repoRoot}, args...)
	cmd := exec.Command("git", full...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// tagFacts holds the git-derived placeholder values computed once per
// resolution. Exposed via name → value map for the substituter.
type tagFacts struct {
	ShortSHA string
	FullSHA  string
	Date     string
	Branch   string
}

func (f tagFacts) lookup(name string) (string, bool) {
	switch name {
	case "short_sha":
		return f.ShortSHA, true
	case "full_sha":
		return f.FullSHA, true
	case "date":
		return f.Date, true
	case "branch":
		return f.Branch, true
	}
	return "", false
}

// collectTagFacts queries git once for every placeholder source. Failure
// of any single query stops resolution — silently substituting an empty
// SHA would produce a bogus tag and corrupt the cache key.
//
// now supplies the date; pass nil for the wall clock.
func collectTagFacts(repoRoot string, git gitRunner, now clock) (tagFacts, error) {
	full, err := git(repoRoot, "rev-parse", "HEAD")
	if err != nil {
		return tagFacts{}, fmt.Errorf("resolve full_sha: %w", err)
	}
	short := AbbrevSHA(full)
	branch, err := git(repoRoot, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return tagFacts{}, fmt.Errorf("resolve branch: %w", err)
	}
	if now == nil {
		now = time.Now
	}
	return tagFacts{
		ShortSHA: short,
		FullSHA:  full,
		Date:     now().UTC().Format(BuildDateLayout),
		Branch:   kebabBranch(branch),
	}, nil
}

// AbbrevSHA truncates a full commit SHA to a fixed width. `git rev-parse
// --short` picks the shortest unambiguous prefix for the *local* object
// database, so the same commit yields a different length on a shallow clone
// than on a full one — two distinct image tags for identical code, and a
// spurious redeploy. Truncating here makes the tag depend only on the commit.
func AbbrevSHA(full string) string {
	if len(full) < ShortSHALen {
		return full
	}
	return full[:ShortSHALen]
}

// kebabBranch rewrites a branch name so it is safe to use as a tag
// suffix: slashes become dashes (`feat/foo` → `feat-foo`).
func kebabBranch(s string) string {
	return strings.ReplaceAll(s, "/", "-")
}

// TagConfigError is returned for static misconfiguration of a
// `tag:` block (both strategy and template set, neither set, or
// unknown strategy). Carrying a typed error lets the compile-failure
// printer attach a precise remediation hint without grepping the
// message.
type TagConfigError struct {
	// Reason is one of:
	//   "both"    — both strategy and template set
	//   "neither" — neither strategy nor template set
	//   "unknown" — strategy value not in the supported list
	Reason string
	// Strategy is populated for Reason="unknown" so the printer can
	// echo the offending value back to the user.
	Strategy string
}

func (e *TagConfigError) Error() string {
	switch e.Reason {
	case "both":
		return "tag: cannot set both strategy and template"
	case "neither":
		return "tag: must set either strategy or template"
	case "unknown":
		return fmt.Sprintf("tag: unknown strategy %q (want one of: %s, %s, %s)",
			e.Strategy, TagStrategyDateSha, TagStrategySha, TagStrategyDate)
	}
	return "tag: invalid configuration"
}

// resolveTag computes the publish tag for the given pipeline. Returns
// an empty string and no error when t is nil (the no-tag default).
// A non-nil error means the pipeline cannot be compiled: misconfigured
// strategy, conflicting Strategy+Template, or git query failure.
func resolveTag(t *TagBlock, repoRoot string, git gitRunner, now clock) (string, error) {
	if t == nil {
		return "", nil
	}
	if t.Strategy != "" && t.Template != "" {
		return "", &TagConfigError{Reason: "both"}
	}
	if t.Strategy == "" && t.Template == "" {
		return "", &TagConfigError{Reason: "neither"}
	}
	if git == nil {
		git = defaultGitRunner
	}
	facts, err := collectTagFacts(repoRoot, git, now)
	if err != nil {
		return "", err
	}
	if t.Strategy != "" {
		return applyStrategy(t.Strategy, facts)
	}
	return substitutePlaceholders(t.Template, facts.lookup), nil
}

func applyStrategy(name string, f tagFacts) (string, error) {
	switch name {
	case TagStrategyDateSha:
		return f.Date + "-" + f.ShortSHA, nil
	case TagStrategySha:
		return f.ShortSHA, nil
	case TagStrategyDate:
		return f.Date, nil
	default:
		return "", &TagConfigError{Reason: "unknown", Strategy: name}
	}
}

// substitutePlaceholders rewrites every `{{ name }}` for which lookup
// returns ok. Unknown names are left literally in place — the user's
// step might intentionally embed a `{{ }}` fragment for another tool
// (helm, gomplate, …) and we must not eat it.
func substitutePlaceholders(s string, lookup func(string) (string, bool)) string {
	if s == "" || !strings.Contains(s, "{{") {
		return s
	}
	return tagPlaceholderRE.ReplaceAllStringFunc(s, func(match string) string {
		sub := tagPlaceholderRE.FindStringSubmatch(match)
		if len(sub) != 2 {
			return match
		}
		if v, ok := lookup(sub[1]); ok {
			return v
		}
		return match
	})
}

// substituteTagPlaceholders is the public substitution helper used by
// CompilePipeline once the tag is resolved. The lookup recognises every
// placeholder collectTagFacts knows about PLUS `publish_tag` (the final
// resolved value). Step Run/Env values are passed through this.
func substituteTagPlaceholders(s string, publishTag string, facts tagFacts) string {
	return substitutePlaceholders(s, func(name string) (string, bool) {
		if name == "publish_tag" {
			return publishTag, true
		}
		return facts.lookup(name)
	})
}
