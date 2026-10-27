package space

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPurgeSpaceCache_DestroysVolumesUnderTheDataRoot(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	data := t.TempDir()
	t.Setenv("SKLP_DATA_ROOT", data)
	cache, err := cacheRoot()
	if err != nil {
		t.Fatal(err)
	}
	scope := newScope("bench", cache)
	vol := filepath.Join(scope.volumesRoot(), "pgdata")
	for _, d := range []string{scope.spaceDir(), vol} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	other := filepath.Join(newScope("other", cache).volumesRoot(), "keep")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := PurgeSpaceCache("bench", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(scope.volumesRoot())); !os.IsNotExist(err) {
		t.Fatalf("the space's data dir survived a volume-destroying purge: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("another space's volume was touched: %v", err)
	}
	if _, err := os.Stat(data); err != nil {
		t.Fatalf("the data root itself was removed: %v", err)
	}
}
