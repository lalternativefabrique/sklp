package pipeline

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ensureRepoCloned materialises repoURL into target when target is
// empty or absent. If target already contains a `.git` directory we
// assume the operator pre-populated it (DockerSpawner bind-mount, or a
// developer running sklp on their laptop) and skip the clone — the
// existing workspace is the source of truth.
//
// headRef is optional. When set we pass `--branch <headRef>` so the
// clone lands on the right ref; when empty, the remote's default
// branch is used (which mirrors `git clone <url>` semantics).
//
// baseRef is optional. When set we fetch it AFTER the initial clone so
// the impact-diff (which compares head against base) has a real commit
// to diff against. Without this step the runner's planner sees an
// empty diff on shallow clones and skips every scope-gated step.
//
// Token handling: repoURL is expected to already carry the
// authentication (e.g. `https://x-access-token:<PAT>@github.com/<org>/<repo>.git`).
// We deliberately do NOT read GITHUB_TOKEN here so the caller stays in
// control of which credential leaks into the workspace.
func ensureRepoCloned(target, repoURL, headRef, baseRef string) error {
	abs, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("abs target: %w", err)
	}
	// Already a git checkout? Leave it alone.
	if _, err := os.Stat(filepath.Join(abs, ".git")); err == nil {
		return nil
	}
	// Refuse to clone over non-empty directories that aren't git
	// checkouts — better to fail loudly than to merge contents.
	if entries, err := os.ReadDir(abs); err == nil && len(entries) > 0 {
		return fmt.Errorf("target %s is not empty and not a git checkout; refusing to clone", abs)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return fmt.Errorf("mkdir target: %w", err)
	}

	// Initial clone of head: shallow so we do not pull the entire repo
	// just to learn what changed. `--depth=50` is a compromise between
	// pulling enough history to find the base ref locally and keeping
	// the clone fast on big repos.
	args := []string{"clone", "--depth=50"}
	if headRef != "" {
		args = append(args, "--branch", headRef)
	}
	args = append(args, repoURL, abs)
	cmd := exec.Command("git", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Redact the token from stderr before surfacing it. The PAT
		// can appear in the URL git prints back when auth fails.
		clean := redactSecret(stderr.String(), repoURL)
		return fmt.Errorf("git clone failed: %w (stderr: %s)", err, strings.TrimSpace(clean))
	}

	// Fetch the base ref so the planner can `git diff <base>...HEAD`.
	// We do NOT check out base — we just need the commit object locally.
	// Failures here are non-fatal: when the base ref doesn't resolve on
	// the remote (e.g. a fresh repo with no main yet) the planner falls
	// back to HEAD~1.
	if baseRef != "" {
		fetchArgs := []string{
			"-C", abs,
			"fetch", "--depth=50", "origin",
			baseRef + ":refs/remotes/origin/" + baseRef,
		}
		fetchCmd := exec.Command("git", fetchArgs...)
		var fetchErr bytes.Buffer
		fetchCmd.Stderr = &fetchErr
		if err := fetchCmd.Run(); err != nil {
			clean := redactSecret(fetchErr.String(), repoURL)
			// Log to stderr (visible in CI logs) without aborting; the
			// pipeline can still execute, just with a degraded diff.
			fmt.Fprintf(os.Stderr, "[self-clone] warn: fetch %s failed (continuing with HEAD~1 fallback): %s\n",
				baseRef, strings.TrimSpace(clean))
		}
	}

	// Normalise file mtimes to the HEAD commit timestamp so that
	// repeated clones of the same commit produce identical mtimes —
	// which is what the Go build cache hashes (see golang/go#58571).
	// Without this, every CI run rebuilds the entire monorepo from
	// scratch even though GOCACHE contains every artefact (each clone
	// gets fresh "now" mtimes, mismatching the cached hash inputs).
	// Best-effort: failure here is logged but never aborts the run —
	// the worst case is the same as before, a colder cache.
	if err := normalizeMtimes(abs); err != nil {
		fmt.Fprintf(os.Stderr, "[self-clone] warn: mtime normalisation failed (cache may not hit): %v\n", err)
	}
	return nil
}

// normalizeMtimes sets every tracked file's mtime to the HEAD commit's
// committer-date. Mirrors the `git restore-mtime` / `git ls-files |
// xargs touch -d @<ct>` recipe from golang/go#58571, in pure Go to
// avoid shelling out for every file on large repos.
func normalizeMtimes(repoDir string) error {
	// 1. HEAD commit time in unix seconds.
	tsBytes, err := exec.Command("git", "-C", repoDir, "log", "-1", "--format=%ct").Output()
	if err != nil {
		return fmt.Errorf("read HEAD commit time: %w", err)
	}
	tsStr := strings.TrimSpace(string(tsBytes))
	tsSec, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return fmt.Errorf("parse commit time %q: %w", tsStr, err)
	}
	commitTime := time.Unix(tsSec, 0)

	// 2. List of tracked files, NUL-separated to survive filenames
	// containing newlines (unusual but cheap to handle).
	listCmd := exec.Command("git", "-C", repoDir, "ls-files", "-z")
	listOut, err := listCmd.Output()
	if err != nil {
		return fmt.Errorf("git ls-files: %w", err)
	}

	// 3. os.Chtimes on each. Symlinks: we don't follow them (their
	// target may be outside the repo); Chtimes on a symlink updates
	// the target, which is fine — but if Lstat tells us it's a
	// symlink we skip it to stay safe.
	for _, rel := range strings.Split(strings.TrimRight(string(listOut), "\x00"), "\x00") {
		if rel == "" {
			continue
		}
		full := filepath.Join(repoDir, rel)
		if fi, err := os.Lstat(full); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			continue
		}
		// Errors on individual files (e.g. submodule placeholders) are
		// not fatal — log nothing, just skip.
		_ = os.Chtimes(full, commitTime, commitTime)
	}
	return nil
}

// redactSecret replaces the userinfo portion of any URL matching
// repoURL with a placeholder so the original PAT does not leak into
// CI logs or error messages persisted to the DB.
func redactSecret(s, repoURL string) string {
	// Trivial heuristic: find any `https://x-access-token:<...>@` in the
	// captured stderr and mask the token. Anchored on the exact prefix
	// so it cannot accidentally mask unrelated `:password@` strings.
	const prefix = "https://x-access-token:"
	// Scan from the current offset so successive matches don't fall
	// back onto the just-rewritten prefix (which would otherwise
	// re-match — `***@...` still contains the prefix — and spin
	// forever; see #379).
	offset := 0
	for {
		i := strings.Index(s[offset:], prefix)
		if i < 0 {
			break
		}
		i += offset
		at := strings.Index(s[i:], "@")
		if at < 0 {
			break
		}
		s = s[:i+len(prefix)] + "***" + s[i+at:]
		// Advance past the rewritten userinfo so the next scan starts
		// after the `***@`.
		offset = i + len(prefix) + len("***") + 1
		if offset > len(s) {
			break
		}
	}
	return s
}

// SelfClone materialises repoURL into target so a caller that holds no
// checkout of its own can run a pipeline against it. It is the exported
// entry point of the same self-clone the -repo-url flag performs: the node
// API (pipeline/runserve) needs it because it clones and runs in two
// separate steps, where the CLI does both inside ParseArgsAndRun.
//
// Idempotent: an existing git checkout at target is left untouched.
// SelfClone is reachable from the run API, so its git arguments come from a
// remote caller: one starting with '-' would be parsed by git as an option.
func SelfClone(target, repoURL, headRef, baseRef string) error {
	for _, v := range []string{repoURL, headRef, baseRef} {
		if strings.HasPrefix(v, "-") {
			return fmt.Errorf("refusing git argument %q: must not start with '-'", redactSecret(v, repoURL))
		}
	}
	return ensureRepoCloned(target, repoURL, headRef, baseRef)
}
