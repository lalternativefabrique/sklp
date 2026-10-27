// pipeline_build_contexts.go classifies the value side of a
// `kind: publish` `build_contexts:` entry so the runner knows
// whether to fold the source into the cache key as a tree (local
// directory) or as a verbatim ref (image, git, OCI, stage). Issue #445.
package pipeline

import (
	"path/filepath"
	"strings"
)

// pathEscapesRepoRoot reports whether a repo-relative path normalises
// to a parent-relative or absolute location — i.e. one that would
// escape the repo root when used as an Inputs glob. The runner's
// existing validator rejects such paths; we mirror that decision here
// so we can skip the Inputs fold-in without surfacing a compile error
// (out-of-repo build_contexts are a legitimate use case, they just
// fall back to "value-only" cache invalidation).
func pathEscapesRepoRoot(p string) bool {
	if p == "" {
		return false
	}
	if filepath.IsAbs(p) {
		return true
	}
	clean := filepath.ToSlash(filepath.Clean(p))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return true
	}
	return false
}

// localDirBuildContext reports whether `value` looks like a local
// directory path that the runner should hash into the cache key. The
// rule is conservative: a value is a local dir when it does NOT match
// any of the BuildKit "remote" prefixes that have a clean ref of their
// own (those flow through unchanged and contribute via the spec's
// JSON marshalling).
//
// Returned `path` is the value unchanged when ok; callers append `/**`
// to use it as a glob.
//
// The list of "remote" prefixes is the public BuildKit set:
//
//   - docker-image://    e.g. docker-image://acme/schemas:v1.2.0
//   - oci-layout://      OCI layout
//   - target:            a named stage in the SAME Dockerfile
//   - http://, https://  URL (BuildKit unpacks the tarball / git repo)
//   - git://             explicit git URL form
//
// Everything else is treated as a local-dir path. This is the same
// classification BuildKit itself uses internally when no scheme is
// present.
func localDirBuildContext(value string) (path string, ok bool) {
	if value == "" {
		return "", false
	}
	// Remote schemes / pseudo-schemes that BuildKit resolves itself.
	// Order: most-specific first so a prefix like "docker-image://" is
	// matched before a hypothetical scheme that shares its prefix.
	for _, p := range remoteBuildContextPrefixes {
		if strings.HasPrefix(value, p) {
			return "", false
		}
	}
	return value, true
}

// remoteBuildContextPrefixes is the set of value prefixes that BuildKit
// resolves on its own (no local-tree hashing needed). Kept in one place
// so future BuildKit additions are a one-line update.
var remoteBuildContextPrefixes = []string{
	"docker-image://",
	"oci-layout://",
	"target:",
	"http://",
	"https://",
	"git://",
	"ssh://",
}
