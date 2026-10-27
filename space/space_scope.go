package space

import (
	"os"
	"path/filepath"

	"github.com/lalternativefabrique/sklp/internal/xdg"
)

// Space is sklp's containerization unit made explicit: a named group of
// services that share a user namespace and a private spacenet bridge (name-DNS),
// run rootless under runc. It is the domain aggregate the deploy/dev paths
// operate on — previously diffuse across spaceName fields, SpaceNet, and
// Space bookkeeping (see docs/sklp/space-domain.md).
//
// This first iteration is deliberately the IDENTITY + SCOPING half of the
// aggregate: a Space owns its Name and is the single source of every host path
// and runc id derived from it. Making these per-space (instead of the global
// constants they were) is what lets two spaces run isolated at once — every
// scoped resource flows through one Space, so no caller can accidentally reach
// into another space's volumes, containers, or state. The network/services/
// lifecycle still live on Space/SpaceNet; the Space is the key they
// all hang off, and the launcher embeds one.
//
// A Space with an empty Name is the STANDALONE space: no bridge, global (unscoped)
// paths — the pre-spacenet behaviour the CI single/multi-container path relies on.
// Scoped accessors fall back to the unscoped layout when Name is empty, so the
// standalone path is byte-identical to before.
type spaceScope struct {
	// name is the space identity and the key to every scoped resource below.
	// Empty = the standalone space (no bridge, unscoped global paths).
	name string
	// cacheRoot is the embedded runtime cache the space's resources live under.
	cacheRoot string
	// dataRoot is the node's data disk, where NAMED VOLUMES live. Empty when the
	// node declares none, which sends volumes back under cacheRoot — the legacy
	// layout, kept for `sklp dev` and for a node that stores nothing durable.
	dataRoot string
}

// spacesDirName is the single cache subdir under which every named space's
// per-space state lives (<cache>/spaces/<name>/...). Centralised so the registry
// scan and the scoped path accessors share ONE source of truth — never a 4th
// hardcoded "spaces" literal.
const spacesDirName = "spaces"

// newSpace binds a space identity to a cache root. The launcher builds one and
// routes every path/id through it, so the scoping policy lives in ONE place.
func newScope(name, cacheRoot string) spaceScope {
	return spaceScope{name: sanitizeSpaceName(name), cacheRoot: cacheRoot, dataRoot: xdg.DataRoot()}
}

// spacesRoot is the cache dir holding every named space's state
// (<cache>/spaces/). The registry lists its entries to discover spaces; the
// scoped accessors join the space name onto it.
func spacesRoot(cacheRoot string) string {
	return filepath.Join(cacheRoot, spacesDirName)
}

// spaceDir is this space's own state dir (<cache>/spaces/<name>/). Empty for the
// standalone space, which uses the legacy flat layout.
func (s spaceScope) spaceDir() string {
	return filepath.Join(spacesRoot(s.cacheRoot), s.name)
}

// childRootfsDir holds this space's own copies of image rootfs, owned by its
// child root: a file can belong to one uid only, and every space has its own.
func (s spaceScope) childRootfsDir() string {
	return filepath.Join(s.spaceDir(), "rootfs")
}

// Name is the space's identity (empty for the standalone space).
func (s spaceScope) Name() string { return s.name }

// standalone reports whether this is the unscoped space (no bridge, global
// paths) — true when no name was given.
func (s spaceScope) standalone() bool { return s.name == "" }

// runcStateRoot is the runc state directory for this space's containers. A
// named space gets its OWN state root so two spaces never share runc state
// (a same-named service in two spaces would otherwise collide on one id /
// reap). The standalone space keeps the legacy shared root for back-compat.
func (s spaceScope) runcStateRoot() string {
	if s.standalone() {
		return filepath.Join(s.cacheRoot, "deploy-runc-state")
	}
	return filepath.Join(s.spaceDir(), "runc-state")
}

// volumesRoot is where this space's NAMED volumes are materialised. Per-space
// so a volume "pgdata" in space A and "pgdata" in space B are DISTINCT host
// directories (invariant I2). The standalone space keeps the legacy flat dir.
func (s spaceScope) volumesRoot() string {
	// A node with a data disk keeps volumes there: they are the one thing under
	// a space that no rebuild recreates, and the cache is what `sklp clean`
	// empties. Without one they stay in the cache, as they always did.
	if s.dataRoot != "" {
		if s.standalone() {
			return filepath.Join(s.dataRoot, "volumes")
		}
		return filepath.Join(s.dataRoot, spacesDirName, s.name, "volumes")
	}
	if s.standalone() {
		return filepath.Join(s.cacheRoot, "volumes")
	}
	return filepath.Join(s.spaceDir(), "volumes")
}

func (s spaceScope) isEmptyShellDir() bool {
	for _, dir := range []string{s.runcStateRoot(), s.volumesRoot(), s.bundleRoot(), s.childRootfsDir()} {
		if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
			return false
		}
	}
	return true
}

// bundleRoot is where this space's per-service OCI bundles + captured logs
// live. Per-space so a same-named service in two spaces (e.g. `web` in both)
// gets a DISTINCT bundle dir and service.log — otherwise their logs interleave
// into one file and leak across spaces (invariant I4, was missed for bundles).
// The standalone space keeps the legacy flat dir.
func (s spaceScope) bundleRoot() string {
	if s.standalone() {
		return filepath.Join(s.cacheRoot, "deploy-bundles")
	}
	return filepath.Join(s.spaceDir(), "deploy-bundles")
}

// containerIDPrefix is the runc-id prefix for this space's containers. A named
// space folds its name into the prefix so its ids are disjoint from another
// space's (invariant I4) AND a reap scoped to this prefix never touches another
// space's containers. The standalone space keeps the legacy global prefix.
func (s spaceScope) containerIDPrefix() string {
	if s.standalone() {
		return deployIDPrefix
	}
	return deployIDPrefix + s.name + "-"
}

// containerID is the stable runc id of a service instance in this space.
func (s spaceScope) containerID(instance string) string {
	return s.containerIDPrefix() + sanitize(instance)
}

// sanitizeSpaceName keeps a space name safe as a path segment and id fragment.
// It reuses the container-id sanitizer (lowercase, [a-z0-9-]), so a space name
// can be embedded in both a filesystem path and a runc id without escaping.
func sanitizeSpaceName(name string) string {
	if name == "" {
		return ""
	}
	return sanitize(name)
}
