package devstack

import "github.com/lalternativefabrique/sklp/internal/layout"

// StackForSpace recovers which dev stack of repoAbs produced the space named
// spaceName. The space name is devScopeID(repo, stack) — a one-way hash — so the
// only honest way back is to hash every candidate and compare. Returns ("",
// false) when no stack of this repo maps to that space.
//
// The empty stack is tried too: it is the legacy single-stack id, still what a
// repo whose dev.yaml sits at the root produces.
func StackForSpace(repoAbs, spaceName string) (string, bool) {
	if devSpaceName(repoAbs, "") == spaceName {
		return "", true
	}
	stacks, err := layout.New(repoAbs).StacksFor("dev")
	if err != nil {
		return "", false
	}
	for _, s := range stacks {
		if devSpaceName(repoAbs, s) == spaceName {
			return s, true
		}
	}
	return "", false
}
