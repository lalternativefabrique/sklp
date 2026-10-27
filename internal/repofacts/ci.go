package repofacts

import (
	"path/filepath"
	"sort"
	"strings"
)

// detectCIHints enumerates existing CI configuration files. We do not
// parse them; the path is enough for the LLM to optionally read the
// file via its own read_file tool and mine for commands.
//
// We list paths relative to root with forward slashes for portable
// JSON output.
func detectCIHints(files []string, root string) CIHints {
	var hints CIHints
	for _, p := range files {
		r := rel(root, p)
		switch {
		case strings.HasPrefix(r, ".github/workflows/") &&
			(strings.HasSuffix(r, ".yml") || strings.HasSuffix(r, ".yaml")):
			hints.GitHubActions = append(hints.GitHubActions, r)
		case filepath.Base(p) == ".gitlab-ci.yml":
			hints.GitLabCI = append(hints.GitLabCI, r)
		case r == ".circleci/config.yml" || r == ".circleci/config.yaml":
			hints.CircleCI = append(hints.CircleCI, r)
		case filepath.Base(p) == ".drone.yml":
			hints.Drone = append(hints.Drone, r)
		}
	}
	// Sort for deterministic JSON output. The LLM's context cache
	// benefits from the same input producing the same prompt.
	sort.Strings(hints.GitHubActions)
	sort.Strings(hints.GitLabCI)
	sort.Strings(hints.CircleCI)
	sort.Strings(hints.Drone)
	return hints
}
