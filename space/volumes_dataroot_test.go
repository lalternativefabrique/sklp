package space

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/xdg"
)

// A node with a data disk must keep named volumes there: they are the one thing
// under a space no rebuild recreates, and the cache is what `sklp clean` empties.
func TestVolumesRoot_UsesDataRootWhenConfigured(t *testing.T) {
	data := t.TempDir()
	t.Setenv(xdg.DataRootEnv, data)

	got := newScope("production", "/cache").volumesRoot()
	if !strings.HasPrefix(got, data) {
		t.Errorf("volumesRoot = %q, want it under the data root %q", got, data)
	}
	if strings.Contains(got, "/cache") {
		t.Errorf("volumesRoot = %q, must not fall back to the cache when a data root exists", got)
	}
	if want := filepath.Join(data, spacesDirName, "production", "volumes"); got != want {
		t.Errorf("volumesRoot = %q, want %q", got, want)
	}
}

// Two spaces must not share a volume directory, whichever root backs it:
// "pgdata" in one space and "pgdata" in another are distinct databases.
func TestVolumesRoot_StaysPerSpaceOnTheDataRoot(t *testing.T) {
	data := t.TempDir()
	t.Setenv(xdg.DataRootEnv, data)

	if a, b := newScope("prod", "/cache").volumesRoot(), newScope("staging", "/cache").volumesRoot(); a == b {
		t.Errorf("two spaces resolved to the same volume root %q", a)
	}
}

// Without a data root the layout is the one every existing node has, so
// upgrading sklp does not move a live database.
func TestVolumesRoot_FallsBackToCacheWithoutDataRoot(t *testing.T) {
	t.Setenv(xdg.DataRootEnv, "")

	got := newScope("production", "/cache").volumesRoot()
	if want := filepath.Join("/cache", spacesDirName, "production", "volumes"); got != want {
		t.Errorf("volumesRoot = %q, want the legacy cache path %q", got, want)
	}
}
