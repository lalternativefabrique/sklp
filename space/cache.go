package space

import (
	"os"
	"path/filepath"
	"strings"
)

// cacheRoot resolves the host cache directory for the embedded
// runtime, honouring $XDG_CACHE_HOME.
func cacheRoot() (string, error) {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".cache")
	}
	root := filepath.Join(base, "sklp", "embedded-runtime")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	return root, nil
}

// centralLogDir is the host per-repo CI log dir the reporter writes into:
// <XDG_CACHE_HOME|~/.cache>/sklp/logs/<repo-basename>. It MUST stay identical to
// pipeline.CentralLogDir — they are the two ends of the same path (the CI runner
// bind-mounts this onto where the inner `sklp run` writes). We can't import
// pipeline here (import cycle via internal/plan), so the logic is mirrored;
// TestCentralLogDir_MatchesPipeline guards the two against drift.
func centralLogDir(repoPath string) string {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		if home, err := os.UserHomeDir(); err == nil {
			base = filepath.Join(home, ".cache")
		} else {
			base = filepath.Join(os.TempDir(), "sklp-cache")
		}
	}
	slug := filepath.Base(strings.TrimRight(repoPath, string(filepath.Separator)))
	if slug == "" || slug == "." || slug == string(filepath.Separator) {
		slug = "unknown"
	}
	return filepath.Join(base, "sklp", "logs", slug)
}

// RuntimeCacheRoot is the cache root the runtime resolves images from — the
// same root NewSpace hands to the launcher. Exposed so a local image install
// lands where a deploy will look for it.
func RuntimeCacheRoot() (string, error) { return cacheRoot() }
