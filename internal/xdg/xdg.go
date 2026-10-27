// Package xdg resolves per-user host directories for sklp's runtime
// state, honouring the XDG base-directory spec. It is the single source
// of truth for "where does sklp put state that belongs to the user and
// the machine, not the repo tree" — control sockets, deploy locks, and
// the dev-stack run-state all live here.
//
// It deliberately depends on nothing but the standard library so any
// package can import it without pulling in the heavy, Linux-/cgo-tagged
// `runtime` package (whose own cacheRoot is embedded-runtime-only). It
// replaces the per-file copies that previously lived in deploy/control.
package xdg

import (
	"os"
	"path/filepath"
)

// CacheRoot is sklp's cache base: $XDG_CACHE_HOME/sklp, or ~/.cache/sklp.
// On a homeless environment it falls back to the OS temp dir so the path
// is always well-formed. Callers append their own subdir (e.g. "dev",
// "deploy-locks"). It does NOT create the directory — callers MkdirAll
// the specific leaf they need with the perms they want.
func CacheRoot() string {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(os.TempDir(), "sklp-cache")
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "sklp")
}

// StateRoot is sklp's persistent per-user state base: $XDG_STATE_HOME/sklp, or
// ~/.local/state/sklp. Unlike CacheRoot this holds data that must SURVIVE a
// cache clean — ACME account keys and issued certs (rate-limited to re-obtain).
// Homeless fallback keeps the path well-formed. Does NOT create the directory.
func StateRoot() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(os.TempDir(), "sklp-state")
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "sklp")
}

// RuntimeRunRoot is the per-user runtime dir for ephemeral sockets/state:
// $XDG_RUNTIME_DIR/sklp (tmpfs, the canonical spot) when available, else
// CacheRoot()/run/sklp as a fallback. Used for control sockets and any
// other short-lived runtime artefact.
func RuntimeRunRoot() string {
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		return filepath.Join(rt, "sklp")
	}
	return filepath.Join(CacheRoot(), "run", "sklp")
}

// DataRootEnv names the directory a node stores deployed spaces' NAMED VOLUMES
// in. `sklet host setup` writes it once, pointing at the host's data disk; the
// supervisor reads it back on every apply.
const DataRootEnv = "SKLP_DATA_ROOT"

// DataRoot is where a deployed space's named volumes live: $SKLP_DATA_ROOT when
// a node declares one, else "" — deliberately NOT a cache path.
//
// A database that lands under CacheRoot is one `sklp clean` away from gone, and
// on a provisioned node it also misses the separate disk that exists so data
// survives the VM. So there is no fallback here: an empty result means the node
// never configured a data disk, and the caller decides what that means (a
// production supervisor refuses to run a space with volumes; a dev stack
// happily keeps them in the cache).
func DataRoot() string {
	return os.Getenv(DataRootEnv)
}
