package space

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/lalternativefabrique/sklp/internal/xdg"
)

// CleanOpts selects which machine-global caches a Clean pass removes. Every
// field defaults to false: the caller opts each scope in. Volumes is held apart
// from the rest because the others are operator-owned and reconstructible (a
// re-pull / re-extract), while volumes hold persistent data owned by a mapped
// subuid — never destroyed implicitly.
type CleanOpts struct {
	// Images clears the OCI image cache (extracted rootfs + blobs + tag→digest
	// pointers). Always operator-owned, safe to drop.
	Images bool
	// Spaces clears every named space's per-space state (spaces/<name>/). Space
	// state may be subuid-owned. This is the only scope that touches deployment
	// state, so callers that must preserve a live deployment set it false and
	// purge idle spaces individually instead.
	Spaces bool
	// BuildCaches clears the reconstructible build caches (runner/<recipe-hash>
	// rootfs builds + ci-toolcache/). These are NEVER deployment state — a
	// rebuild recreates them — so they are safe to drop even while a deployment
	// runs. Split from Spaces so `sklp clean --images` can reclaim these (often the
	// largest caches) without wiping any space's state.
	BuildCaches bool
	// Volumes also removes named volumes — DESTROYS persistent data (Postgres
	// DB, NATS store). Orthogonal to the scopes above.
	Volumes bool
}

// CleanResult reports what a Clean pass removed (or could not).
type CleanResult struct {
	// RemovedImages is true when the image cache (extracted rootfs + blobs +
	// tag→digest pointers) was cleared.
	RemovedImages bool
	// RemovedSpaces is true when per-space state was cleared.
	RemovedSpaces bool
	// RemovedBuildCaches is true when the runner rootfs builds + ci-toolcache
	// were cleared.
	RemovedBuildCaches bool
	// VolumesSkipped lists named volumes left in place because the caller did
	// not opt into removing them (they hold persistent data — a Postgres DB,
	// a NATS store — that a routine cache clean must not destroy).
	VolumesSkipped []string
	// VolumeErrors maps a volume dir to the error that stopped its removal,
	// typically a subuid-owned tree the operator cannot delete directly.
	VolumeErrors map[string]error
}

// CleanCache is the legacy entrypoint: clear the image cache, optionally the
// volumes. Retained so `dev clean`'s historical behaviour is one call.
func CleanCache(removeVolumes bool) (CleanResult, error) {
	return Clean(CleanOpts{Images: true, Volumes: removeVolumes})
}

// purgeAllSpaceOverlays drops the overlay scratch of every materialised space,
// leaving volumes/ and the remaining space state untouched. Called when images
// are cleared, since each overlay's lowerdir is one of those images.
//
// A missing spaces/ dir is not an error — nothing was ever materialised.
func purgeAllSpaceOverlays(root string) error {
	entries, err := os.ReadDir(filepath.Join(root, spacesDirName))
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, spacesDirName, e.Name(), "overlay")
		if _, serr := os.Stat(dir); serr != nil {
			continue
		}
		// subuid-aware: the upper is owned by mapped uids when the container ran
		// as non-root, so a plain remove hits EPERM.
		if rerr := subidRemoveAll(dir); rerr != nil {
			return fmt.Errorf("clean overlay for space %s: %w", e.Name(), rerr)
		}
	}
	return nil
}

// Clean purges the selected machine-global caches under the embedded runtime
// root. operator-owned caches (images, space rootfs builds) are a plain
// RemoveAll — the next run re-pulls/re-extracts. Clearing images also drops
// every space's overlay scratch, whose lowerdir those images are; see
// purgeAllSpaceOverlays. Named volumes are
// touched ONLY when opts.Volumes is set: they hold persistent data and are
// owned by a mapped subuid (a container ran as a non-root user), so deleting
// them both loses data AND requires privilege the operator may not have outside
// a userns. A volume it cannot delete (subuid-owned) is reported in
// VolumeErrors rather than failing the whole clean, so the rest still clears.
func Clean(opts CleanOpts) (CleanResult, error) {
	root, err := cacheRoot()
	if err != nil {
		return CleanResult{}, err
	}
	res := CleanResult{VolumeErrors: map[string]error{}}

	if opts.Images {
		// Drop every service overlay FIRST. A service rootfs is an overlayfs
		// whose lowerdir is the extracted image under rootfs/ — deleting the
		// image without the overlay leaves a mount pointing at a lower that no
		// longer exists, and the next run starts the container on a near-empty
		// merged dir. The failure surfaces far from here, as the image's
		// entrypoint "not found in $PATH" even though it is present on disk.
		//
		// Only overlay/ is removed: the upper is ephemeral by design (discarding
		// it IS the image-immutability guarantee), while volumes/ and the rest of
		// the space state survive — so this stays safe for callers preserving a
		// live deployment, which is exactly why Spaces is not implied instead.
		if err := purgeAllSpaceOverlays(root); err != nil {
			return res, err
		}
		// An extracted rootfs keeps the owners its layers declare, subuids
		// the operator cannot unlink as itself.
		for _, sub := range []string{"rootfs", "blobs", "refs"} {
			if err := subidRemoveAll(filepath.Join(root, sub)); err != nil {
				return res, fmt.Errorf("clean %s: %w", sub, err)
			}
		}
		res.RemovedImages = true
	}

	if opts.BuildCaches {
		// runner/ and ci-toolcache/ hold subuid-owned files (go mod cache, build
		// artefacts written by container uid 0); image-push/ is `sklp image push`
		// scratch. Route through subidRemoveAll so the userns remap lets us unlink
		// them. All reconstructible, never deployment state, so safe to drop
		// independently of Spaces.
		for _, sub := range []string{"runner", "ci-toolcache", "image-push"} {
			if err := subidRemoveAll(filepath.Join(root, sub)); err != nil {
				return res, fmt.Errorf("clean %s: %w", sub, err)
			}
		}
		res.RemovedBuildCaches = true
	}

	if opts.Spaces {
		// spaces/<name>/ holds per-space state; may be subuid-owned.
		if err := subidRemoveAll(filepath.Join(root, spacesDirName)); err != nil {
			return res, fmt.Errorf("clean %s: %w", spacesDirName, err)
		}
		res.RemovedSpaces = true
	}

	cleanVolumes(root, opts, &res)
	return res, nil
}

// cleanVolumes walks the named volumes of every space and removes them when
// opts.Volumes is set. Volumes live at spaces/<space>/volumes/<name> — the same
// path volumeNames() reports — NOT under a flat dev-volumes/ root: reading the
// wrong root made -v a silent no-op, since a missing dir yields no entries and
// the loop then had nothing to skip OR delete, so neither VolumesSkipped nor
// VolumeErrors ever reported that the data was still there.
func cleanVolumes(root string, opts CleanOpts, res *CleanResult) {
	// A node with a data disk keeps its volumes there, outside every path this
	// function walks. Deleting them is then not clean's to do: the data root
	// exists precisely so a cache clean cannot reach a production database, and
	// a space is torn down through `space down`/`purge`, which knows its own
	// volume root.
	if xdg.DataRoot() != "" {
		return
	}
	spaces, err := os.ReadDir(filepath.Join(root, spacesDirName))
	if err != nil {
		return
	}
	for _, s := range spaces {
		if !s.IsDir() {
			continue
		}
		volRoot := filepath.Join(root, spacesDirName, s.Name(), "volumes")
		entries, err := os.ReadDir(volRoot)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			// Qualified by space: two spaces may each hold a "pgdata", and an
			// operator deciding whether to pass -v must see which one is at stake.
			name := s.Name() + "/" + e.Name()
			if !opts.Volumes {
				res.VolumesSkipped = append(res.VolumesSkipped, name)
				continue
			}
			if err := subidRemoveAll(filepath.Join(volRoot, e.Name())); err != nil {
				// A subuid-owned tree even a userns remap could not delete (e.g. no
				// newuidmap); report it, keep going so the rest still clears.
				res.VolumeErrors[name] = err
			}
		}
		if opts.Volumes {
			pruneEmptySpaceDir(filepath.Join(root, spacesDirName, s.Name()))
		}
	}
}

// pruneEmptySpaceDir drops a space directory that holds nothing but an empty
// volumes/. List enumerates the spaces/ subdirs, so a space whose last volume
// was just deleted would otherwise keep reappearing in `space ls` and in the
// cockpit with no way to remove it — teardown keeps the dir for the volumes,
// and -v deleted the volumes but not their now-empty container.
func pruneEmptySpaceDir(dir string) {
	volRoot := filepath.Join(dir, "volumes")
	if entries, err := os.ReadDir(volRoot); err != nil || len(entries) > 0 {
		return
	}
	if err := os.Remove(volRoot); err != nil {
		return
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
		os.Remove(dir)
	}
}
