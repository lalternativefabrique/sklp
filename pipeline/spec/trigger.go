package spec

import "path"

// BranchAllowed reports whether branch satisfies a TriggerBlock's
// branch list.
//
// Patterns are matched in order and the LAST match decides, so a
// negation can carve an exception out of a wildcard:
//
//	['**', '!main']        every branch but main
//	[main]                 main only
//	['release/**', '!release/**-rc']
//
// A nil block, an empty list, or an empty branch (no git context)
// allows everything: a pipeline that says nothing about branches runs
// anywhere, which is what an unrestricted CI wants.
func BranchAllowed(t *TriggerBlock, branch string) bool {
	if t == nil {
		return true
	}
	pats := t.Branches
	// never: excludes whatever the branch is — including with no git
	// context at all, where every branch-shaped rule abstains.
	if len(pats) == 0 && t.Never {
		return false
	}
	if branch == "" {
		return true
	}
	if len(pats) == 0 {
		return legacyBranchAllowed(t, branch)
	}
	allowed := hasOnlyNegations(pats)
	for _, p := range pats {
		neg := len(p) > 0 && p[0] == '!'
		if neg {
			p = p[1:]
		}
		if matchBranch(p, branch) {
			allowed = !neg
		}
	}
	return allowed
}

// hasOnlyNegations seeds the decision for a list made exclusively of
// exclusions. ['!main'] means "anywhere but main", so the default has
// to be allow; any positive pattern flips the default back to deny.
func hasOnlyNegations(pats []string) bool {
	for _, p := range pats {
		if len(p) == 0 || p[0] != '!' {
			return false
		}
	}
	return true
}

// matchBranch matches one pattern against a branch name. "**" spans
// slashes; path.Match handles the rest.
func matchBranch(pattern, branch string) bool {
	if pattern == "**" {
		return true
	}
	ok, err := path.Match(pattern, branch)
	if err != nil {
		return false
	}
	if ok {
		return true
	}
	return matchDoubleStar(pattern, branch)
}

// matchDoubleStar handles a "prefix/**" pattern, which path.Match
// treats as a single path element and so fails on nested refs.
func matchDoubleStar(pattern, branch string) bool {
	const star = "/**"
	if len(pattern) <= len(star) || pattern[len(pattern)-len(star):] != star {
		return false
	}
	prefix := pattern[:len(pattern)-len(star)]
	return len(branch) > len(prefix) && branch[:len(prefix)] == prefix && branch[len(prefix)] == '/'
}

// legacyBranchAllowed reads the pre-branches spelling so a pipeline
// written against except:/never: keeps its meaning until it migrates.
func legacyBranchAllowed(t *TriggerBlock, branch string) bool {
	if t.Never {
		return false
	}
	for _, b := range t.Except {
		if b == branch {
			return false
		}
	}
	return true
}
