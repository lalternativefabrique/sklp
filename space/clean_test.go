package space

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCleanCache_ImagesOnly clears the image cache (rootfs/blobs/refs) but
// keeps named volumes by default.
func TestCleanCache_ImagesOnly(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)
	base := filepath.Join(root, "sklp", "embedded-runtime")
	mustMkFile(t, filepath.Join(base, "rootfs", "abc", "f"))
	mustMkFile(t, filepath.Join(base, "blobs", "b"))
	mustMkFile(t, filepath.Join(base, "refs", "tag"))
	mustMkFile(t, filepath.Join(base, spacesDirName, "dev", "volumes", "pgdata", "f"))

	res, err := CleanCache(false)
	if err != nil {
		t.Fatalf("CleanCache: %v", err)
	}
	if !res.RemovedImages {
		t.Error("RemovedImages must be true")
	}
	for _, sub := range []string{"rootfs", "blobs", "refs"} {
		if _, err := os.Stat(filepath.Join(base, sub)); !os.IsNotExist(err) {
			t.Errorf("%s should be removed", sub)
		}
	}
	// Volume kept + reported.
	if _, err := os.Stat(filepath.Join(base, spacesDirName, "dev", "volumes", "pgdata")); err != nil {
		t.Error("named volume must be kept by default")
	}
	if len(res.VolumesSkipped) != 1 || res.VolumesSkipped[0] != "dev/pgdata" {
		t.Errorf("VolumesSkipped = %v, want [dev/pgdata]", res.VolumesSkipped)
	}
}

// TestCleanCache_WithVolumes removes operator-owned volumes too. The volumes
// live at spaces/<space>/volumes/<name>, the layout the registry actually
// writes: reading a flat dev-volumes/ root instead made -v delete nothing while
// still reporting success.
func TestCleanCache_WithVolumes(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)
	base := filepath.Join(root, "sklp", "embedded-runtime")
	mustMkFile(t, filepath.Join(base, spacesDirName, "dev", "volumes", "natsdata", "f"))

	res, err := CleanCache(true)
	if err != nil {
		t.Fatalf("CleanCache(true): %v", err)
	}
	if len(res.VolumesSkipped) != 0 {
		t.Errorf("nothing should be skipped with removeVolumes, got %v", res.VolumesSkipped)
	}
	if _, err := os.Stat(filepath.Join(base, spacesDirName, "dev", "volumes", "natsdata")); !os.IsNotExist(err) {
		t.Error("operator-owned volume should be removed with --volumes")
	}
}

// A space whose last volume is deleted must not survive as an empty shell: List
// enumerates spaces/ subdirs, so it would reappear in `space ls` and the cockpit
// with no remaining way to remove it.
func TestCleanCache_WithVolumesDropsEmptiedSpaceDir(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)
	base := filepath.Join(root, "sklp", "embedded-runtime")
	spaceDir := filepath.Join(base, spacesDirName, "dev")
	mustMkFile(t, filepath.Join(spaceDir, "volumes", "pgdata", "f"))

	if _, err := CleanCache(true); err != nil {
		t.Fatalf("CleanCache(true): %v", err)
	}
	if _, err := os.Stat(spaceDir); !os.IsNotExist(err) {
		t.Error("an emptied space dir must be removed, not left as a shell")
	}
}

// A space still holding state (bundles, runc-state) keeps its dir even when its
// volumes go: only the volumes were asked for.
func TestCleanCache_WithVolumesKeepsSpaceDirWithState(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)
	base := filepath.Join(root, "sklp", "embedded-runtime")
	spaceDir := filepath.Join(base, spacesDirName, "dev")
	mustMkFile(t, filepath.Join(spaceDir, "volumes", "pgdata", "f"))
	mustMkFile(t, filepath.Join(spaceDir, "runc-state", "state.json"))

	if _, err := CleanCache(true); err != nil {
		t.Fatalf("CleanCache(true): %v", err)
	}
	if _, err := os.Stat(spaceDir); err != nil {
		t.Error("a space dir with surviving state must be kept")
	}
}

// TestClean_SpacesScope clears per-space state (spaces/) only — NOT the runner
// build cache (now under BuildCaches) nor the image cache.
func TestClean_SpacesScope(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)
	base := filepath.Join(root, "sklp", "embedded-runtime")
	mustMkFile(t, filepath.Join(base, "runner", "deadbeef", "rootfs", "bin", "sh")) // build cache, must survive a spaces-only clean
	mustMkFile(t, filepath.Join(base, spacesDirName, "dev", "state.json"))
	mustMkFile(t, filepath.Join(base, "rootfs", "abc", "f")) // image cache, must survive

	res, err := Clean(CleanOpts{Spaces: true})
	if err != nil {
		t.Fatalf("Clean spaces: %v", err)
	}
	if !res.RemovedSpaces {
		t.Error("RemovedSpaces must be true")
	}
	if _, err := os.Stat(filepath.Join(base, spacesDirName)); !os.IsNotExist(err) {
		t.Errorf("%s should be removed", spacesDirName)
	}
	if _, err := os.Stat(filepath.Join(base, "runner", "deadbeef")); err != nil {
		t.Error("runner build cache must survive a spaces-only clean (it is BuildCaches now)")
	}
	if res.RemovedImages {
		t.Error("RemovedImages must stay false when only Spaces is selected")
	}
	if _, err := os.Stat(filepath.Join(base, "rootfs", "abc")); err != nil {
		t.Error("image cache must survive a spaces-only clean")
	}
}

// TestClean_BuildCachesScope clears the reconstructible build caches
// (runner/, ci-toolcache/, image-push/) while leaving per-space state and the
// image cache untouched — the split that lets `sklp clean --images` reclaim the big
// caches without wiping a live deployment's state.
func TestClean_BuildCachesScope(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)
	base := filepath.Join(root, "sklp", "embedded-runtime")
	mustMkFile(t, filepath.Join(base, "runner", "deadbeef", "rootfs", "bin", "sh"))
	mustMkFile(t, filepath.Join(base, "ci-toolcache", "go", "mod"))
	mustMkFile(t, filepath.Join(base, "image-push", "work", "layer"))
	mustMkFile(t, filepath.Join(base, spacesDirName, "dev", "state.json")) // per-space state, must survive
	mustMkFile(t, filepath.Join(base, "rootfs", "abc", "f"))               // image cache, must survive

	res, err := Clean(CleanOpts{BuildCaches: true})
	if err != nil {
		t.Fatalf("Clean build caches: %v", err)
	}
	if !res.RemovedBuildCaches {
		t.Error("RemovedBuildCaches must be true")
	}
	for _, sub := range []string{"runner", "ci-toolcache", "image-push"} {
		if _, err := os.Stat(filepath.Join(base, sub)); !os.IsNotExist(err) {
			t.Errorf("%s should be removed", sub)
		}
	}
	if _, err := os.Stat(filepath.Join(base, spacesDirName, "dev")); err != nil {
		t.Error("per-space state must survive a build-caches-only clean")
	}
	if _, err := os.Stat(filepath.Join(base, "rootfs", "abc")); err != nil {
		t.Error("image cache must survive a build-caches-only clean")
	}
	if res.RemovedSpaces || res.RemovedImages {
		t.Error("only RemovedBuildCaches should be set")
	}
}

func mustMkFile(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestClean_ImagesDropsSpaceOverlays is the regression test for a stale-lower
// overlay: clearing images must also drop each space's overlay scratch, whose
// lowerdir is one of those images. Leaving it behind starts the next container
// on a near-empty merged dir, surfacing as the image entrypoint being "not
// found in $PATH" while it is plainly on disk.
func TestClean_ImagesDropsSpaceOverlays(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)
	base := filepath.Join(root, "sklp", "embedded-runtime")
	mustMkFile(t, filepath.Join(base, "rootfs", "digest1", "usr", "local", "bin", "entrypoint.sh"))
	// Two spaces, each with a service overlay and a named volume.
	for _, sp := range []string{"ci-skalpai", "dev-local"} {
		mustMkFile(t, filepath.Join(base, "spaces", sp, "overlay", "db", "merged", "x"))
		mustMkFile(t, filepath.Join(base, "spaces", sp, "overlay", "db", "upper", "y"))
		mustMkFile(t, filepath.Join(base, "spaces", sp, "volumes", "pgdata", "keepme"))
	}

	if _, err := Clean(CleanOpts{Images: true}); err != nil {
		t.Fatalf("Clean: %v", err)
	}

	for _, sp := range []string{"ci-skalpai", "dev-local"} {
		if _, err := os.Stat(filepath.Join(base, "spaces", sp, "overlay")); !os.IsNotExist(err) {
			t.Errorf("space %s: overlay must be removed with the images it overlays", sp)
		}
		// Per-space persistent data must survive: this is why images does NOT
		// simply imply Spaces.
		if _, err := os.Stat(filepath.Join(base, "spaces", sp, "volumes", "pgdata", "keepme")); err != nil {
			t.Errorf("space %s: volumes must survive an images-only clean: %v", sp, err)
		}
	}
}

// Spaces with no overlay materialised, or no spaces dir at all, must not fail.
func TestClean_ImagesWithoutOverlaysIsNoop(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)
	base := filepath.Join(root, "sklp", "embedded-runtime")
	mustMkFile(t, filepath.Join(base, "rootfs", "d", "f"))
	mustMkFile(t, filepath.Join(base, "spaces", "bare", "volumes", "v", "f"))

	if _, err := Clean(CleanOpts{Images: true}); err != nil {
		t.Fatalf("Clean with no overlay: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "spaces", "bare", "volumes", "v", "f")); err != nil {
		t.Errorf("volumes must survive: %v", err)
	}

	// No spaces/ dir at all.
	root2 := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root2)
	mustMkFile(t, filepath.Join(root2, "sklp", "embedded-runtime", "rootfs", "d", "f"))
	if _, err := Clean(CleanOpts{Images: true}); err != nil {
		t.Fatalf("Clean with no spaces dir: %v", err)
	}
}

// A spaces-only clean must not touch images.
func TestClean_SpacesOnlyKeepsImages(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)
	base := filepath.Join(root, "sklp", "embedded-runtime")
	mustMkFile(t, filepath.Join(base, "rootfs", "digest1", "f"))
	mustMkFile(t, filepath.Join(base, "spaces", "s1", "overlay", "db", "upper", "y"))

	if _, err := Clean(CleanOpts{Spaces: true}); err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "rootfs", "digest1", "f")); err != nil {
		t.Errorf("images must survive a spaces-only clean: %v", err)
	}
}
