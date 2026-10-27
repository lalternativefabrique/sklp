package control

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

// TestRemoveSpaceArtifacts: after a down, the per-space dir (manifest +
// pidfile) is gone, so a reboot's __supervise finds no manifest and stays down.
// Idempotent: a second removal on a missing dir is not an error.
func TestRemoveSpaceArtifacts(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	svcs := []spacemanager.Service{{Name: "api", Image: "img:1"}}
	meta := spacemanager.SpaceMeta{Name: "production"}
	if err := SaveManifest("production", svcs, meta); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(ManifestPath("production")); err != nil {
		t.Fatalf("manifest not written: %v", err)
	}

	if err := RemoveSpaceArtifacts("production"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(ManifestPath("production")); !os.IsNotExist(err) {
		t.Fatalf("manifest still present after removal: %v", err)
	}
	// A space with no manifest no longer loads -> reboot stays down.
	if _, _, err := LoadManifest("production"); err == nil {
		t.Fatal("LoadManifest succeeded after artifacts removed; want error (space stays down)")
	}
	// Idempotent second call.
	if err := RemoveSpaceArtifacts("production"); err != nil {
		t.Fatalf("second remove: %v", err)
	}
}

// TestManifestRoundTrip: SaveManifest then LoadManifest returns the same
// resolved services + meta, under the deterministic per-space path.
func TestManifestRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	svcs := []spacemanager.Service{
		{Name: "api", Image: "img:1", Replicas: 2, Port: 8080},
		{Name: "web", Image: "web:1"},
	}
	meta := spacemanager.SpaceMeta{Name: "production", URL: "https://prod.example"}

	if err := SaveManifest("production", svcs, meta); err != nil {
		t.Fatalf("save: %v", err)
	}
	gotSvcs, gotMeta, err := LoadManifest("production")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(gotSvcs) != 2 || gotSvcs[0].Name != "api" || gotSvcs[0].Replicas != 2 || gotSvcs[0].Port != 8080 {
		t.Fatalf("services = %+v", gotSvcs)
	}
	if gotSvcs[1].Name != "web" {
		t.Fatalf("services[1] = %+v", gotSvcs[1])
	}
	if gotMeta.Name != "production" || gotMeta.URL != "https://prod.example" {
		t.Fatalf("meta = %+v", gotMeta)
	}
}

// TestManifestPathDeterministic: the path is ~/.config (XDG_CONFIG_HOME) /
// sklp/spaces/<space>/manifest.json — stable, cwd-independent.
func TestManifestPathDeterministic(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	want := filepath.Join(dir, "sklp", "spaces", "staging", "manifest.json")
	if got := ManifestPath("staging"); got != want {
		t.Fatalf("ManifestPath = %q, want %q", got, want)
	}
}

// TestManifestDirPerms: the per-space config dir is created 0700 (owner-only),
// the manifest file 0600.
func TestManifestDirPerms(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	if err := SaveManifest("production", []spacemanager.Service{{Name: "api", Image: "i:1"}}, spacemanager.SpaceMeta{Name: "production"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	dir := filepath.Join(root, "sklp", "spaces", "production")
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Fatalf("dir perm = %o, want 0700", perm)
	}
	fi, err := os.Stat(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("file perm = %o, want 0600", perm)
	}
}

// TestManifestAtomicOverwrite: a second SaveManifest replaces the first cleanly
// (no temp leftovers, latest content wins).
func TestManifestAtomicOverwrite(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)

	if err := SaveManifest("production", []spacemanager.Service{{Name: "api", Image: "v1"}}, spacemanager.SpaceMeta{Name: "production"}); err != nil {
		t.Fatalf("save v1: %v", err)
	}
	if err := SaveManifest("production", []spacemanager.Service{{Name: "api", Image: "v2"}}, spacemanager.SpaceMeta{Name: "production"}); err != nil {
		t.Fatalf("save v2: %v", err)
	}
	gotSvcs, _, err := LoadManifest("production")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if gotSvcs[0].Image != "v2" {
		t.Fatalf("image = %q, want v2 (overwrite)", gotSvcs[0].Image)
	}
	// No temp file leftovers in the space dir.
	dir := filepath.Join(root, "sklp", "spaces", "production")
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "manifest.json" {
			t.Fatalf("unexpected leftover file %q in space dir", e.Name())
		}
	}
}

// TestLoadManifestMissing: loading a never-saved space is a clear error, not a
// zero-value success.
func TestLoadManifestMissing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, _, err := LoadManifest("nope"); err == nil {
		t.Fatal("want error loading a missing manifest, got nil")
	}
}
