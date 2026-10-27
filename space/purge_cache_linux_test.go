//go:build linux

package space

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/xdg"
)

// PurgeSpaceCache(keepVolumes=true) is what `sklp deploy delete` uses — it has
// no `-v` equivalent and has never dropped named volumes, so this must remove
// the leaked deploy-bundles/runc-state siblings PurgeSpaceScratch alone left
// behind (found live on node-a, 2026-08-30) while still preserving
// volumes under the legacy no-data-root layout (spaceDir/volumes).
func TestPurgeSpaceCache_KeepVolumesPreservesVolumesNoDataRoot(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv(xdg.DataRootEnv, "")

	root, err := cacheRoot()
	if err != nil {
		t.Fatal(err)
	}
	scope := newScope("api2", root)
	dir := scope.spaceDir()

	mustMkdirAll(t, filepath.Join(dir, "overlay", "api"))
	mustMkdirAll(t, filepath.Join(dir, "deploy-bundles", "svc-api-0"))
	mustMkdirAll(t, filepath.Join(dir, "runc-state"))
	mustMkdirAll(t, filepath.Join(dir, "volumes", "pgdata"))
	mustWriteFile(t, filepath.Join(dir, "volumes", "pgdata", "data.db"), "important")

	if err := PurgeSpaceCache("api2", true); err != nil {
		t.Fatalf("PurgeSpaceCache: %v", err)
	}

	for _, gone := range []string{"overlay", "deploy-bundles", "runc-state"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Errorf("%s survived a keepVolumes purge, want it removed (err=%v)", gone, err)
		}
	}
	data := filepath.Join(dir, "volumes", "pgdata", "data.db")
	if _, err := os.Stat(data); err != nil {
		t.Errorf("named volume data was removed despite keepVolumes=true: %v", err)
	}
}

// A data-root node keeps volumes OUTSIDE spaceDir entirely (dataRoot/spaces/
// <name>/volumes) — this is the branch where the per-entry walk and a single
// subidRemoveAll(spaceDir) would behave identically, but the walk must not
// accidentally skip anything just because a sibling directory happens to also
// be named "volumes" under an unrelated path.
func TestPurgeSpaceCache_KeepVolumesWithDataRootRemovesSpaceDirVolumesToo(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	dataRoot := t.TempDir()
	t.Setenv(xdg.DataRootEnv, dataRoot)

	root, err := cacheRoot()
	if err != nil {
		t.Fatal(err)
	}
	scope := newScope("api2", root)
	dir := scope.spaceDir()

	// Under a data root, volumesRoot() resolves OUTSIDE spaceDir, so a
	// directory named "volumes" left inside spaceDir (e.g. a leftover from
	// before the data root was configured) is not the real named-volume
	// store and must be purged like any other cache entry.
	mustMkdirAll(t, filepath.Join(dir, "runc-state"))
	mustMkdirAll(t, filepath.Join(dir, "volumes"))
	mustMkdirAll(t, scope.volumesRoot())
	mustWriteFile(t, filepath.Join(scope.volumesRoot(), "pgdata-marker"), "real volume store")

	if err := PurgeSpaceCache("api2", true); err != nil {
		t.Fatalf("PurgeSpaceCache: %v", err)
	}

	for _, gone := range []string{"runc-state", "volumes"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Errorf("%s under spaceDir survived the purge (err=%v)", gone, err)
		}
	}
	if _, err := os.Stat(scope.volumesRoot()); err != nil {
		t.Errorf("the real (data-root) volume store was removed: %v", err)
	}
}

// keepVolumes=false (space down -v / purge's contract) must remove everything,
// volumes included.
func TestPurgeSpaceCache_DropVolumesRemovesEverything(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv(xdg.DataRootEnv, "")

	root, err := cacheRoot()
	if err != nil {
		t.Fatal(err)
	}
	dir := newScope("api2", root).spaceDir()
	mustMkdirAll(t, filepath.Join(dir, "volumes", "pgdata"))
	mustMkdirAll(t, filepath.Join(dir, "runc-state"))

	if err := PurgeSpaceCache("api2", false); err != nil {
		t.Fatalf("PurgeSpaceCache: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("spaceDir survived a keepVolumes=false purge (err=%v)", err)
	}
}

// A space with no materialised cache dir at all (never deployed, or already
// purged) is a clean no-op, not an error — mirrors PurgeSpaceScratch's own
// contract for a missing dir.
func TestPurgeSpaceCache_MissingSpaceDirIsNoop(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv(xdg.DataRootEnv, "")

	if err := PurgeSpaceCache("never-deployed", true); err != nil {
		t.Fatalf("PurgeSpaceCache on a missing space dir: %v", err)
	}
}

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The space inventory lists spaces by DIRECTORY NAME under spaces/, so a purge
// that empties the dir but leaves it behind creates a ghost the UI shows
// forever (found live: 5 ghost spaces on node-b after deploy deletes).
// With a data root (volumes elsewhere) the dir must disappear entirely; with
// in-dir volumes kept, it must remain.
func TestPurgeSpaceCache_RemovesEmptiedSpaceDir(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	dataRoot := t.TempDir()
	t.Setenv(xdg.DataRootEnv, dataRoot)

	root, err := cacheRoot()
	if err != nil {
		t.Fatal(err)
	}
	scope := newScope("ghost", root)
	dir := scope.spaceDir()
	mustMkdirAll(t, filepath.Join(dir, "overlay", "svc"))
	mustMkdirAll(t, filepath.Join(dir, "runc-state"))

	if err := PurgeSpaceCache("ghost", true); err != nil {
		t.Fatalf("PurgeSpaceCache: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("emptied space dir survived the purge and would list as a ghost (err=%v)", err)
	}
}

// The counterpart: volumes living INSIDE the space dir (no data root) must
// keep the dir alive — removing it would take the data with it.
func TestPurgeSpaceCache_KeepsDirWhenInDirVolumesRemain(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv(xdg.DataRootEnv, "")

	root, err := cacheRoot()
	if err != nil {
		t.Fatal(err)
	}
	scope := newScope("keeper", root)
	dir := scope.spaceDir()
	mustMkdirAll(t, filepath.Join(dir, "overlay"))
	mustMkdirAll(t, filepath.Join(dir, "volumes", "pgdata"))
	mustWriteFile(t, filepath.Join(dir, "volumes", "pgdata", "data.db"), "x")

	if err := PurgeSpaceCache("keeper", true); err != nil {
		t.Fatalf("PurgeSpaceCache: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "volumes", "pgdata", "data.db")); err != nil {
		t.Fatalf("in-dir volume lost: %v", err)
	}
}
