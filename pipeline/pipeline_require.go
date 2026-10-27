// Package runner — pipeline-level require: block evaluation.
//
// The require: block declares fail-fast preconditions that the engine
// enforces BEFORE compiling any step. This is what lets a publish
// pipeline drop the boilerplate `check-main-branch` / `check-clean-tree`
// steps: the engine performs the same checks, declaratively, at a
// guaranteed point in the run.
//
// Invariant (load-bearing): when a require check fails, NO step has
// been compiled, NO service has been started, NO tool has been
// resolved, NO Docker daemon has been touched. The only side effect
// is the `git` subprocess used to read branch / status.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/lalternativefabrique/sklp/pipeline/spec"
)

// ErrRequireViolation is returned when a pipeline's require: block
// rejects the current environment. Callers can use errors.Is to
// distinguish a precondition failure from a compile / runtime error.
var ErrRequireViolation = errors.New("require violation")

// Rule identifiers carried on RequireViolationError. Stable strings so
// JSON consumers and the human-banner renderer can branch on them.
const (
	RequireRuleBranches  = "branches"
	RequireRuleCleanTree = "clean_tree"
)

// RequireViolationError carries the precondition failure detail.
// Always wraps ErrRequireViolation so the sentinel-based checks work.
//
// The structured fields exist so the CLI can render a clear, actionable
// banner (and a JSON object in -output json mode) without re-parsing
// the message string. Message itself stays a one-liner suitable for
// log lines and the existing `Error() string` contract.
type RequireViolationError struct {
	// Message is the one-line summary surfaced via Error().
	Message string

	// Rule is the failed precondition's identifier — one of the
	// RequireRule* constants. Drives both the banner template and
	// the `details.rule` field in JSON-mode output.
	Rule string

	// Current describes the observed state (e.g. "feat/foo",
	// "1 unstaged, 0 untracked"). Used in the banner's first line.
	Current string

	// Want describes the required state (e.g. "[main]"). Used in the
	// banner to make the gap between current/required obvious.
	Want string

	// PorcelainLines, populated for clean_tree violations, lists the
	// raw `git status --porcelain` entries so the banner can show the
	// user exactly which paths are blocking the run.
	PorcelainLines []string

	// Fix is the one-line remediation hint ("commit or stash, then
	// re-run", "switch to one of the allowed branches, then re-run").
	Fix string
}

func (e *RequireViolationError) Error() string { return e.Message }
func (e *RequireViolationError) Unwrap() error { return ErrRequireViolation }

// requireGitRunner abstracts the two `git` calls EvaluateRequire makes
// so tests can drive the checker without forking real subprocesses
// when needed. Production callers use the zero value (which shells out
// to the system `git`).
type requireGitRunner struct {
	// run, when non-nil, replaces the default exec.CommandContext path.
	// Test seam only.
	run func(ctx context.Context, repo string, args ...string) (string, error)
}

func (r requireGitRunner) exec(ctx context.Context, repo string, args ...string) (string, error) {
	if r.run != nil {
		return r.run(ctx, repo, args...)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// EvaluateRequire enforces a pipeline's require: block against repoPath.
// A nil block is a no-op (today's default). A non-nil block triggers
// one `git` call per enabled check; the first failing check returns a
// RequireViolationError and short-circuits the rest.
func EvaluateRequire(ctx context.Context, block *RequireBlock, repoPath string) error {
	return evaluateRequire(ctx, block, repoPath, requireGitRunner{})
}

func evaluateRequire(ctx context.Context, block *RequireBlock, repoPath string, git requireGitRunner) error {
	if block == nil {
		return nil
	}
	if len(block.Branches) > 0 {
		current, err := currentBranch(ctx, repoPath, git)
		if err != nil {
			return &RequireViolationError{
				Message: fmt.Sprintf("refusing to run: cannot determine current branch: %v", err),
				Rule:    RequireRuleBranches,
				Fix:     "ensure repo is on a named branch, then re-run",
			}
		}
		if !contains(block.Branches, current) {
			want := "[" + strings.Join(block.Branches, " ") + "]"
			return &RequireViolationError{
				Message: fmt.Sprintf("refusing to run: branch '%s' not in require.branches %s",
					current, want),
				Rule:    RequireRuleBranches,
				Current: current,
				Want:    want,
				Fix:     "switch to one of the allowed branches, then re-run",
			}
		}
	}
	if block.CleanTree {
		return requireCleanTree(ctx, repoPath, git)
	}
	return nil
}

// currentBranch reads HEAD's symbolic ref, falling back to a short SHA
// on detached HEAD so the message in the caller stays meaningful.
func currentBranch(ctx context.Context, repo string, git requireGitRunner) (string, error) {
	out, err := git.exec(ctx, repo, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// workingTreeStatus returns counts of (unstaged-or-staged-modified,
// untracked) entries reported by `git status --porcelain`, plus the
// raw porcelain lines themselves. The split keeps the summary useful
// ("3 unstaged, 1 untracked") and the per-path list lets the CLI show
// the user exactly which files are blocking the run.
func workingTreeStatus(ctx context.Context, repo string, git requireGitRunner) (int, int, []string, error) {
	out, err := git.exec(ctx, repo, "status", "--porcelain")
	if err != nil {
		return 0, 0, nil, err
	}
	var unstaged, untracked int
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 2 {
			continue
		}
		lines = append(lines, line)
		// Porcelain v1 format: XY <path> where "??" means untracked.
		if line[0] == '?' && line[1] == '?' {
			untracked++
			continue
		}
		unstaged++
	}
	return unstaged, untracked, lines, nil
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// EvaluateTrigger enforces the `on:` block on the hand-run path, where
// selection never happened: `sklp run publish` names the pipeline, so
// nothing has consulted its branch list yet.
//
// Gating both paths from one list is the point. Splitting them let a
// pipeline be filtered out of a push while staying runnable by hand
// from the wrong branch, and nothing reported the gap.
func EvaluateTrigger(ctx context.Context, block *spec.TriggerBlock, repoPath string) error {
	return evaluateTrigger(ctx, block, repoPath, requireGitRunner{})
}

func evaluateTrigger(ctx context.Context, block *spec.TriggerBlock, repoPath string, git requireGitRunner) error {
	if block == nil {
		return nil
	}
	if len(block.Branches) > 0 {
		current, err := currentBranch(ctx, repoPath, git)
		if err != nil {
			return &RequireViolationError{
				Message: fmt.Sprintf("refusing to run: cannot determine current branch: %v", err),
				Rule:    RequireRuleBranches,
				Fix:     "ensure repo is on a named branch, then re-run",
			}
		}
		if !spec.BranchAllowed(block, current) {
			want := "[" + strings.Join(block.Branches, " ") + "]"
			return &RequireViolationError{
				Message: fmt.Sprintf("refusing to run: branch '%s' does not match on.branches %s",
					current, want),
				Rule:    RequireRuleBranches,
				Current: current,
				Want:    want,
				Fix:     "switch to a branch the pipeline allows, then re-run",
			}
		}
	}
	if block.CleanTree {
		return requireCleanTree(ctx, repoPath, git)
	}
	return nil
}

// requireCleanTree is the clean_tree check shared by the `on:` and the
// deprecated `require:` blocks.
func requireCleanTree(ctx context.Context, repoPath string, git requireGitRunner) error {
	unstaged, untracked, lines, err := workingTreeStatus(ctx, repoPath, git)
	if err != nil {
		return &RequireViolationError{
			Message: fmt.Sprintf("refusing to run: cannot read working tree status: %v", err),
			Rule:    RequireRuleCleanTree,
			Fix:     "ensure the repo is a valid git checkout, then re-run",
		}
	}
	if unstaged > 0 || untracked > 0 {
		status := fmt.Sprintf("%d unstaged, %d untracked", unstaged, untracked)
		return &RequireViolationError{
			Message:        fmt.Sprintf("refusing to run: working tree dirty (%s)", status),
			Rule:           RequireRuleCleanTree,
			Current:        status,
			Want:           "clean working tree",
			PorcelainLines: lines,
			Fix:            "commit or stash, then re-run",
		}
	}
	return nil
}
