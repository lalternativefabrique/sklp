package spec

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	imagebuild "github.com/lalternativefabrique/sklp/pipeline/imagebuild"
)

// changedFiles returns the list of files that differ between the
// current HEAD and baseRef, in the repository rooted at repoPath. The
// caller is responsible for the base ref convention; PipelineRunOptions
// surfaces it so spawners can pass the GitHub PR base, the local
// `origin/main`, or anything else they decide is the comparison point.
//
// When baseRef is empty, we default to "HEAD~1" — same convention as
// runner/resolve.go for the legacy publish.yaml impact diff. That keeps
// the local "run sklp ci on this branch" UX intuitive without a flag.
//
// When git itself fails (no git in PATH, no commits, ref unknown), we
// degrade open: an empty list means "no scope gate can hide a step"
// because everything looks unchanged → every step runs. That's the
// safe default — we'd rather over-run than silently skip a needed
// step due to a broken environment.
func ChangedFiles(ctx context.Context, repoPath, baseRef string) []string {
	if baseRef == "" {
		baseRef = "HEAD~1"
	}
	// git diff --name-only <base>..HEAD prints one path per line,
	// repo-relative, with `/` separators on every platform.
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "diff", "--name-only", baseRef+"...HEAD")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	files := make([]string, 0, len(lines))
	for _, l := range lines {
		if l != "" {
			files = append(files, l)
		}
	}
	return files
}

// scopeMatches returns true when at least one file in changed matches
// at least one glob in any of the named scopes. The semantics are:
//
//	OR across named scopes  (Step.Scope = [a, b] → a OR b)
//	OR across globs in a scope  (scopes[a] = [g1, g2] → g1 OR g2)
//	OR across changed files     (any file that matches → match)
//
// An unknown scope name is a configuration error surfaced to the
// caller, not a silent miss. A step with no scope (or with an empty
// list) bypasses this function entirely from the executor side.
func ScopeMatches(stepScopes []string, scopeDefs map[string][]string, changed []string) (bool, error) {
	if len(stepScopes) == 0 {
		return true, nil // no gate → run
	}
	if len(changed) == 0 {
		// Nothing changed → the safe choice is to run scoped steps
		// anyway. This matches the "no base ref / git failure" case
		// above: we prefer over-running to silent skips.
		return true, nil
	}

	// Pre-collect the unique glob list across the requested scopes so
	// each changed file is matched at most O(globs) times even with
	// large changesets.
	var globs []string
	for _, name := range stepScopes {
		patterns, ok := scopeDefs[name]
		if !ok {
			return false, fmt.Errorf("scope %q referenced by a step is not declared in `scopes:`", name)
		}
		globs = append(globs, patterns...)
	}

	for _, f := range changed {
		for _, g := range globs {
			ok, err := doublestar.PathMatch(g, f)
			if err != nil {
				return false, fmt.Errorf("invalid glob %q: %w", g, err)
			}
			if ok {
				return true, nil
			}
		}
	}
	return false, nil
}

// expandScopesToGlobs returns the deduplicated, lex-sorted union of
// the glob patterns belonging to the named scopes. Unknown scope names
// produce a clear error so a typo in a step's `scope:` does not
// silently degrade into "no implicit inputs". A nil / empty scopeNames
// input yields a nil result.
//
// Used by CompilePipeline to derive implicit cache inputs from a step's
// scope when no explicit `inputs:` is declared. The semantics are
// identical to scopeMatches' OR-across-scopes / OR-across-globs view:
// any file matching any glob in any of the named scopes is treated as
// a content dependency of the step.
func ExpandScopesToGlobs(scopeDefs map[string][]string, scopeNames []string) ([]string, error) {
	if len(scopeNames) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{})
	for _, name := range scopeNames {
		patterns, ok := scopeDefs[name]
		if !ok {
			return nil, fmt.Errorf("scope %q referenced by a step is not declared in `scopes:`", name)
		}
		for _, p := range patterns {
			if p == "" {
				continue
			}
			seen[p] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(seen))
	for g := range seen {
		out = append(out, g)
	}
	sort.Strings(out)
	return out, nil
}

// dockerContextInputs returns the repo-relative files that make up a docker
// build context for cache-key derivation, walking ctxRel under repoRoot but
// SKIPPING anything .dockerignore excludes, plus .git and .claude (worktrees /
// agent state) which are never part of an image yet hold huge node_modules
// trees. df is the Dockerfile path (always included). This replaces a naive
// `<ctx>/**` glob whose descent into node_modules/.git took minutes and bloated
// the key with bytes that never ship.
func DockerContextInputs(repoRoot, ctxRel, df string) ([]string, error) {
	ctxAbs := filepath.Join(repoRoot, ctxRel)
	ig := loadDockerignore(filepath.Join(ctxAbs, ".dockerignore"))

	seen := map[string]struct{}{}
	if df != "" {
		seen[filepath.ToSlash(filepath.Clean(df))] = struct{}{}
	}
	// A non-existent context dir contributes no files — only the Dockerfile,
	// already recorded above. The previous `<ctx>/**` glob matched nothing in
	// that case (no error); preserve that tolerance so a forward-looking step
	// (context created by an earlier step, or simply absent at compile) does not
	// fail compile on a missing directory.
	if _, statErr := os.Stat(ctxAbs); os.IsNotExist(statErr) {
		out := make([]string, 0, len(seen))
		for p := range seen {
			out = append(out, p)
		}
		sort.Strings(out)
		return out, nil
	}
	err := filepath.WalkDir(ctxAbs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(ctxAbs, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		base := d.Name()
		// Always prune VCS + agent/worktree state — never in an image, huge.
		if d.IsDir() && (base == ".git" || base == ".claude" || base == "node_modules") {
			return filepath.SkipDir
		}
		if rel != "." && ig != nil && ig.Ignored(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if fi, e := d.Info(); e != nil || !fi.Mode().IsRegular() {
			return nil
		}
		// Record as a repo-relative path (Inputs are repo-rooted).
		repoRel, e := filepath.Rel(repoRoot, p)
		if e != nil {
			return nil
		}
		seen[filepath.ToSlash(repoRel)] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan build context %q: %w", ctxRel, err)
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// loadDockerignore reads a .dockerignore matcher, nil when absent.
func loadDockerignore(path string) *imagebuild.Ignorer {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return imagebuild.ParseDockerignore(string(b))
}

// inputPruneDirs are directories wildcard input expansion never descends
// into: VCS/agent state plus regenerated package stores, which hold circular
// symlinks (pnpm) and vendored sources that must never key a cache.
var inputPruneDirs = map[string]struct{}{
	".git":         {},
	".claude":      {},
	"node_modules": {},
	".pnpm-store":  {},
}

// pruneFS hides inputPruneDirs from ReadDir so `**` never walks them.
// Literal path segments bypass ReadDir and still resolve normally.
type pruneFS struct{ fs.FS }

func (p pruneFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(p.FS, name)
	if err != nil {
		return nil, err
	}
	kept := entries[:0]
	for _, e := range entries {
		if _, skip := inputPruneDirs[e.Name()]; skip {
			continue
		}
		kept = append(kept, e)
	}
	return kept, nil
}

// resolveInputGlobs expands one or more doublestar globs against repoRoot into a
// deduplicated, lex-sorted list of repo-relative file paths (regular files only).
// WithNoFollow: a circular symlink (pnpm store → repo root) would otherwise
// make `**` re-walk the tree until ELOOP and duplicate every input path.
func ResolveInputGlobs(repoRoot string, globs []string) ([]string, error) {
	if len(globs) == 0 {
		return nil, nil
	}
	root := fs.FS(pruneFS{os.DirFS(repoRoot)})
	seen := make(map[string]struct{})
	for _, g := range globs {
		matches, err := doublestar.Glob(root, g, doublestar.WithFilesOnly(), doublestar.WithNoFollow())
		if err != nil {
			return nil, fmt.Errorf("glob %q: %w", g, err)
		}
		for _, m := range matches {
			// Defence-in-depth: skip anything that doublestar somehow
			// returned outside the FS root or that is not a regular file.
			info, err := fs.Stat(root, m)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			seen[filepath.ToSlash(m)] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}
