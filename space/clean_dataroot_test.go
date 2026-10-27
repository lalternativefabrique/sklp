package space

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/xdg"
)

// The data root exists so a cache clean cannot reach a production database.
// Even `-v`, the flag whose whole job is deleting volumes, must not touch it.
func TestCleanVolumes_NeverTouchesTheDataRoot(t *testing.T) {
	cache := t.TempDir()
	data := t.TempDir()
	t.Setenv(xdg.DataRootEnv, data)

	// A database on the data disk, and a stale copy in the legacy cache layout.
	pgdata := filepath.Join(data, spacesDirName, "production", "volumes", "pgdata")
	if err := os.MkdirAll(pgdata, 0o700); err != nil {
		t.Fatal(err)
	}
	cached := filepath.Join(cache, spacesDirName, "production", "volumes", "pgdata")
	if err := os.MkdirAll(cached, 0o700); err != nil {
		t.Fatal(err)
	}

	res := &CleanResult{VolumeErrors: map[string]error{}}
	cleanVolumes(cache, CleanOpts{Volumes: true}, res)

	if _, err := os.Stat(pgdata); err != nil {
		t.Errorf("clean -v deleted a volume on the data root: %v", err)
	}
	if len(res.VolumesSkipped) != 0 {
		t.Errorf("clean reported volumes it does not own: %v", res.VolumesSkipped)
	}
}

// Without a data root the legacy behaviour is unchanged: volumes live in the
// cache, and clean still owns them.
func TestCleanVolumes_StillCleansTheCacheWithoutDataRoot(t *testing.T) {
	cache := t.TempDir()
	t.Setenv(xdg.DataRootEnv, "")

	vol := filepath.Join(cache, spacesDirName, "dev", "volumes", "pgdata")
	if err := os.MkdirAll(vol, 0o700); err != nil {
		t.Fatal(err)
	}

	res := &CleanResult{VolumeErrors: map[string]error{}}
	cleanVolumes(cache, CleanOpts{}, res)

	if len(res.VolumesSkipped) != 1 || res.VolumesSkipped[0] != "dev/pgdata" {
		t.Errorf("VolumesSkipped = %v, want [dev/pgdata]", res.VolumesSkipped)
	}
}
