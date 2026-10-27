package runnerimage

import (
	"os"
	"path/filepath"

	"github.com/lalternativefabrique/sklp/internal/layout"
)

// Canonical and legacy recipe file names under .sklp/. space.yaml is the recipe
// that defines the closed environment (base image + toolchain + packages + skt
// tools) a project runs in. It was historically called config.yaml; that name
// is still read for backward compat and auto-migrated on the spot.
const (
	spaceFile  = "space.yaml"
	legacyFile = "config.yaml"
)

// ResolvePath returns the path to a project's space recipe under <repoRoot>/.sklp,
// in priority order:
//
//  1. the flat .sklp/space.yaml — the canonical, repo-global recipe;
//  2. the legacy flat .sklp/config.yaml — auto-migrated to space.yaml in place;
//  3. a stack-scoped .sklp/<stack>/space.yaml when exactly one stack carries one
//     and no flat recipe exists — lets a single-stack repo keep the recipe next
//     to its other contracts (dev.yaml, deploy.yaml) instead of at the root.
//
// When none exists the canonical flat space.yaml path is returned so callers
// produce a "space.yaml not found"-style error, not a confusing legacy one. The
// recipe stays ONE per repo: if several stacks declare a space.yaml the flat
// path is returned (ambiguous — the caller must place the recipe at the root).
func ResolvePath(repoRoot string) string {
	sklp := filepath.Join(repoRoot, ".sklp")
	space := filepath.Join(sklp, spaceFile)
	if fileExists(space) {
		return space
	}
	legacy := filepath.Join(sklp, legacyFile)
	if fileExists(legacy) {
		// Auto-migrate: rename in place. Best-effort — if the rename fails
		// (read-only fs, race) fall back to reading the legacy file directly.
		if err := os.Rename(legacy, space); err == nil {
			return space
		}
		return legacy
	}
	if stacked := resolveStackSpace(repoRoot); stacked != "" {
		return stacked
	}
	return space
}

// resolveStackSpace returns the path of the single stack-scoped space recipe
// (.sklp/<stack>/space.yaml) when exactly one stack declares one, else "". Zero
// or many matches are NOT stack-resolved: zero has no recipe, and many is
// ambiguous (the recipe is repo-global, so it cannot live in two stacks at once)
// — both fall back to the flat path so the error/behaviour stays predictable.
func resolveStackSpace(repoRoot string) string {
	lay := layout.New(repoRoot)
	stacks, err := lay.StacksFor("space")
	if err != nil || len(stacks) != 1 {
		return ""
	}
	path, err := lay.Resolve(stacks[0], "space")
	if err != nil {
		return ""
	}
	return path
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}
