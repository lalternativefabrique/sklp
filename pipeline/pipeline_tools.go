package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	// Blank import: every tool's init() registers it with the toolchain
	// package. Importing this once from the runner makes the registry
	// non-empty for downstream callers (sklp CLI, core/ci spawner).
	_ "github.com/lalternativefabrique/sklp/internal/toolchain/tools"
)

// ensureToolsCache picks the per-host cache directory and makes sure
// it exists. Returned path is what the toolResolver hands to the
// native installer (which extracts under <cacheDir>/<tool>/<version>/).
//
// MkdirAll is called once per pipeline run regardless of how many
// tools we ultimately install — cheap, and lets a misconfigured K8s
// mount surface as a clear error before any tool is touched.
func ensureToolsCache() (string, error) {
	cacheDir := toolsCacheDir()
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("toolchain cache dir %s: %w", cacheDir, err)
	}
	return cacheDir, nil
}

// toolsCacheDir is where the toolchain installer materialises binaries.
// Respects SKLP_TOOLS_CACHE for explicit override (the DockerSpawner
// will set it to a per-project volume mount). Falls back to
// XDG_CACHE_HOME/sklp/tools, then $HOME/.cache/sklp/tools, then a
// process-temp dir if even that is unavailable.
func toolsCacheDir() string {
	if v := os.Getenv("SKLP_TOOLS_CACHE"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return filepath.Join(v, "sklp", "tools")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".cache", "sklp", "tools")
	}
	return filepath.Join(os.TempDir(), "sklp-tools")
}

// prependPath returns env with one or more directories prepended to
// the PATH variable. Duplicate directories already on PATH are kept
// in their new (leftmost) position so the toolchain binary always
// wins over a system version.
func prependPath(env []string, dirs []string) []string {
	if len(dirs) == 0 {
		return env
	}
	const key = "PATH="
	prefix := strings.Join(dirs, string(os.PathListSeparator))
	for i, kv := range env {
		if strings.HasPrefix(kv, key) {
			env[i] = key + prefix + string(os.PathListSeparator) + kv[len(key):]
			return env
		}
	}
	return append(env, key+prefix)
}
