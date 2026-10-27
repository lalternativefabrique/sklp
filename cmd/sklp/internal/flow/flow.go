// Package flow implements the feature lifecycle git plumbing behind
// `sklp flow start/end`: open a feature branch (optionally in a dedicated
// worktree) and, at the end, diff the branch against a target ref so the CI
// runner can scope tests to the impacted services.
//
// This package owns ONLY git mechanics + path policy. It never runs the CI
// itself — `flow end` hands the target ref to the existing runner via
// `-base-ref` (see the flow.go command glue). Keeping the seam here means the
// lifecycle adds no new pipeline kind or YAML field; it orchestrates briques
// that already exist.
package flow

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// DefaultPrefix is prepended to a feature name to form the branch (feat/<name>).
const DefaultPrefix = "feat/"

// worktreeRoot returns where dedicated feature worktrees live: a sibling of the
// repo named "<repo>-worktrees" (the repo convention), derived from the repo
// toplevel so it is portable across machines rather than hardcoded.
func worktreeRoot() (string, error) {
	top, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	top = strings.TrimSpace(top)
	return filepath.Join(filepath.Dir(top), filepath.Base(top)+"-worktrees"), nil
}

// StartOptions configures `flow start`.
type StartOptions struct {
	Name       string // feature name (branch becomes feat/<name>)
	Onto       string // ref the branch starts from (default "main")
	BranchOnly bool   // checkout in place instead of creating a worktree
}

// StartResult reports what `flow start` created.
type StartResult struct {
	Branch   string // e.g. "feat/login"
	Worktree string // worktree path, or "" in --branch mode
}

// EndOptions configures `flow end`.
type EndOptions struct {
	Onto string // diff target (default "main")
}

// EndResult reports the diff scope `flow end` resolved.
type EndResult struct {
	Branch  string   // current branch
	Onto    string   // resolved target ref
	Changed []string // files changed vs Onto
}

// Start opens the feature: it creates branch feat/<name> from Onto, either in a
// dedicated worktree (default) or via an in-place checkout (--branch).
func Start(opts StartOptions) (StartResult, error) {
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		return StartResult{}, errors.New("feature name is required")
	}
	onto := orMain(opts.Onto)
	branch := DefaultPrefix + name

	if opts.BranchOnly {
		if _, err := git("checkout", "-b", branch, onto); err != nil {
			return StartResult{}, err
		}
		return StartResult{Branch: branch}, nil
	}

	// Worktree mode: slug the branch into a directory name (feat/login →
	// feat-login) under the conventional root, then add the worktree.
	root, err := worktreeRoot()
	if err != nil {
		return StartResult{}, err
	}
	dir := filepath.Join(root, strings.ReplaceAll(branch, "/", "-"))
	if _, err := os.Stat(dir); err == nil {
		return StartResult{}, fmt.Errorf("worktree path already exists: %s", dir)
	}
	if _, err := git("worktree", "add", "-b", branch, dir, onto); err != nil {
		return StartResult{}, err
	}
	return StartResult{Branch: branch, Worktree: dir}, nil
}

// End resolves the diff scope of the current branch against Onto. It does NOT
// run the CI — the caller feeds EndResult.Onto to the runner as -base-ref. End
// only computes and reports the changed-file set so the summary is honest about
// what will be tested.
func End(opts EndOptions) (EndResult, error) {
	onto := orMain(opts.Onto)

	branch, err := git("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return EndResult{}, err
	}
	branch = strings.TrimSpace(branch)

	// Verify the target ref exists before diffing, so the failure is a clear
	// "unknown target" rather than an opaque git diff error.
	if _, err := git("rev-parse", "--verify", "--quiet", onto); err != nil {
		return EndResult{}, fmt.Errorf("target ref %q not found", onto)
	}

	// Three-dot diff: changes on the branch since it diverged from onto,
	// ignoring commits added to onto in the meantime — the same scope the CI
	// impact diff cares about.
	out, err := git("diff", "--name-only", onto+"...HEAD")
	if err != nil {
		return EndResult{}, err
	}
	return EndResult{Branch: branch, Onto: onto, Changed: nonEmptyLines(out)}, nil
}

// Inspect is the read-only twin of End for monitors (sklp cockpit): it resolves
// the diff scope of the current branch against Onto WITHOUT any side effect. It
// is a pure wrapper over End — End already only diffs (it never runs CI), so
// this just gives callers an intention-revealing, clearly read-only entry point.
func Inspect(onto string) (EndResult, error) {
	return End(EndOptions{Onto: onto})
}

// Worktree returns the absolute path of the current git worktree, or "" if it
// cannot be resolved. Read-only; used by the cockpit flow banner.
func Worktree() string {
	out, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// Push pushes the current branch to origin, setting upstream. Used by
// `flow end --pr` only after the CI passes.
func Push(branch string) error {
	_, err := git("push", "-u", "origin", branch)
	return err
}

// OpenPR opens a pull request from head into base via the GitHub API
// (`gh api .../pulls`, never `gh pr create`, per repo convention). When an
// open PR already targets head, the push that preceded this call has already
// updated it, so the existing URL is returned with created=false instead of
// letting GitHub reject the duplicate with a 422. Title defaults to the
// branch's last commit subject when empty.
func OpenPR(head, base, title string) (url string, created bool, err error) {
	slug, err := originSlug()
	if err != nil {
		return "", false, err
	}
	if existing, err := existingPRURL(slug, head); err != nil {
		return "", false, err
	} else if existing != "" {
		return existing, false, nil
	}
	if strings.TrimSpace(title) == "" {
		subj, err := git("log", "-1", "--pretty=%s")
		if err != nil {
			return "", false, err
		}
		title = strings.TrimSpace(subj)
	}
	out, err := gh("api", "repos/"+slug+"/pulls", "-X", "POST",
		"-f", "title="+title, "-f", "head="+head, "-f", "base="+base)
	if err != nil {
		return "", false, err
	}
	// The API returns JSON; surface html_url if jq-less parsing is easy,
	// otherwise return the raw body for the caller to print.
	if url := jsonField(out, "html_url"); url != "" {
		return url, true, nil
	}
	return strings.TrimSpace(out), true, nil
}

// existingPRURL returns the html_url of the open PR whose head is branch, or
// "" when none exists. A lookup failure is not fatal: the caller falls through
// to the create call, which reports the real error.
func existingPRURL(slug, branch string) (string, error) {
	owner := slug
	if i := strings.Index(slug, "/"); i >= 0 {
		owner = slug[:i]
	}
	out, err := gh("api", "repos/"+slug+"/pulls",
		"-X", "GET", "-f", "state=open", "-f", "head="+owner+":"+branch)
	if err != nil {
		return "", nil
	}
	return firstPRURL(out), nil
}

// firstPRURL pulls html_url out of a GitHub pull-request list response. An
// empty JSON array means no PR targets the branch.
func firstPRURL(body string) string {
	if strings.TrimSpace(body) == "" || strings.TrimSpace(body) == "[]" {
		return ""
	}
	return jsonField(body, "html_url")
}

// originSlug reads the origin remote URL and parses it into "owner/repo".
func originSlug() (string, error) {
	url, err := git("config", "--get", "remote.origin.url")
	if err != nil {
		return "", err
	}
	return parseSlug(url)
}

// parseSlug derives "owner/repo" from a github remote URL (ssh or https). Pure
// string logic, kept separate from git so it is testable without a repo.
func parseSlug(url string) (string, error) {
	url = strings.TrimSpace(url)
	url = strings.TrimSuffix(url, ".git")
	// git@github.com:owner/repo  → owner/repo
	if i := strings.Index(url, ":"); strings.HasPrefix(url, "git@") && i >= 0 {
		return url[i+1:], nil
	}
	// https://github.com/owner/repo → owner/repo
	if i := strings.Index(url, "github.com/"); i >= 0 {
		return url[i+len("github.com/"):], nil
	}
	return "", fmt.Errorf("cannot derive owner/repo from origin url %q", url)
}

// jsonField extracts a top-level string field from a JSON object without a
// full parse — sufficient for "html_url" in the gh response.
func jsonField(body, key string) string {
	needle := "\"" + key + "\""
	i := strings.Index(body, needle)
	if i < 0 {
		return ""
	}
	rest := body[i+len(needle):]
	c := strings.Index(rest, ":")
	if c < 0 {
		return ""
	}
	rest = strings.TrimSpace(rest[c+1:])
	if len(rest) == 0 || rest[0] != '"' {
		return ""
	}
	rest = rest[1:]
	if e := strings.Index(rest, "\""); e >= 0 {
		return rest[:e]
	}
	return ""
}

func orMain(ref string) string {
	if strings.TrimSpace(ref) == "" {
		return "main"
	}
	return ref
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// git runs a git command in the current working directory and returns trimmed
// stdout, or an error carrying stderr for a useful message.
func git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}

// gh runs a gh CLI command and returns trimmed stdout, or an error carrying
// stderr. Used for PR creation via `gh api .../pulls`.
func gh(args ...string) (string, error) {
	cmd := exec.Command("gh", args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("gh %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}
