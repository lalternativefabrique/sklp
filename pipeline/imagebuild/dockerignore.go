package imagebuild

import (
	"path"
	"strings"
)

// Ignorer matches build-context paths against .dockerignore patterns. Docker's
// rules: patterns are matched against the path RELATIVE to the context root,
// using Go path.Match-style globbing extended with `**` (any depth); a leading
// `!` negates (re-includes); the LAST matching pattern wins. A path is ignored
// iff its last matching pattern is non-negated. Comments (#) and blank lines are
// skipped. We intentionally cover the common subset real .dockerignore files use
// (`.git`, `node_modules`, `*.log`, `**/foo`, `!keep`), not every Docker corner.
type Ignorer struct {
	patterns []ignorePattern
}

type ignorePattern struct {
	pat     string
	negated bool
}

// ParseDockerignore builds an Ignorer from a .dockerignore file's content. An
// empty/absent file yields an Ignorer that ignores nothing.
func ParseDockerignore(content string) *Ignorer {
	ig := &Ignorer{}
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		neg := false
		if strings.HasPrefix(line, "!") {
			neg = true
			line = strings.TrimSpace(line[1:])
		}
		// Normalise: strip a leading "./" and any trailing slash (a dir pattern
		// matches the dir and its contents).
		line = strings.TrimPrefix(line, "./")
		line = strings.TrimSuffix(line, "/")
		if line == "" {
			continue
		}
		ig.patterns = append(ig.patterns, ignorePattern{pat: line, negated: neg})
	}
	return ig
}

// Ignored reports whether a context-relative path (slash-separated, no leading
// "./") should be excluded from the build context. The last matching pattern
// decides; a negated last match re-includes. A path under an ignored directory
// is ignored unless a later `!` pattern re-includes it specifically.
func (ig *Ignorer) Ignored(rel string) bool {
	rel = strings.TrimPrefix(filepathClean(rel), "./")
	ignored := false
	for _, p := range ig.patterns {
		if matchPattern(p.pat, rel) {
			ignored = !p.negated
		}
	}
	return ignored
}

// matchPattern matches a .dockerignore pattern against rel. It handles `**`
// (match across directory separators) and otherwise defers to path.Match per
// segment. A pattern also matches everything UNDER a matched directory
// (pattern "node_modules" ignores "node_modules/x/y").
func matchPattern(pat, rel string) bool {
	if directMatch(pat, rel) {
		return true
	}
	// Directory-prefix match: pattern "node_modules" should match
	// "node_modules/anything".
	if strings.HasPrefix(rel, pat+"/") {
		return true
	}
	return false
}

// directMatch implements glob match with `**` support, segment by segment.
func directMatch(pat, name string) bool {
	if !strings.Contains(pat, "**") {
		ok, _ := path.Match(pat, name)
		if ok {
			return true
		}
		// `*.log` should also match nested `a/b.log`? Docker matches patterns
		// against the FULL relative path, so a bare `*.log` matches only top
		// level; nested needs `**/*.log`. Keep path.Match semantics here.
		return false
	}
	return matchDoubleStar(pat, name)
}

// matchDoubleStar handles patterns containing `**` by splitting on it and
// checking the prefix/suffix with path.Match-able segments. Supports the common
// forms: `**/x`, `x/**`, `a/**/b`.
func matchDoubleStar(pat, name string) bool {
	parts := strings.SplitN(pat, "**", 2)
	prefix := strings.TrimSuffix(parts[0], "/")
	suffix := strings.TrimPrefix(parts[1], "/")

	// `**/suffix`: suffix may appear at any depth.
	if prefix == "" {
		if suffix == "" {
			return true // bare `**`
		}
		// match suffix against the tail of every depth.
		segs := strings.Split(name, "/")
		for i := range segs {
			cand := strings.Join(segs[i:], "/")
			if ok, _ := path.Match(suffix, cand); ok {
				return true
			}
		}
		return false
	}
	// `prefix/**` (everything under prefix) or `prefix/**/suffix`.
	if !strings.HasPrefix(name, prefix+"/") && name != prefix {
		return false
	}
	if suffix == "" {
		return true
	}
	rest := strings.TrimPrefix(name, prefix+"/")
	return matchDoubleStar("**/"+suffix, rest)
}

// filepathClean is path.Clean restricted to slash paths (the context is always
// slash-separated here).
func filepathClean(p string) string {
	if p == "" {
		return "."
	}
	return path.Clean(p)
}
