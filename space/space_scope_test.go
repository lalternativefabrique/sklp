package space

import (
	"strings"
	"testing"
)

// Two named spaces must derive DISTINCT bundle roots, runc state roots, and id
// prefixes for a same-named service — otherwise `web` in space A and `web` in
// space B share one service.log and their logs interleave / leak across spaces.
func TestScope_PerSpaceIsolation(t *testing.T) {
	a := newScope("bakery", "/cache")
	b := newScope("shop", "/cache")

	if a.bundleRoot() == b.bundleRoot() {
		t.Errorf("bundleRoot collides across spaces: %q", a.bundleRoot())
	}
	if a.runcStateRoot() == b.runcStateRoot() {
		t.Errorf("runcStateRoot collides across spaces: %q", a.runcStateRoot())
	}
	if a.volumesRoot() == b.volumesRoot() {
		t.Errorf("volumesRoot collides across spaces: %q", a.volumesRoot())
	}
	if a.containerID("web") == b.containerID("web") {
		t.Errorf("containerID collides across spaces: %q", a.containerID("web"))
	}
	// Each scoped path must carry the space name.
	if !strings.Contains(a.bundleRoot(), "bakery") {
		t.Errorf("bundleRoot missing space name: %q", a.bundleRoot())
	}
}

// Every scoped path must sit UNDER the single spacesRoot()/spaceDir() so the
// registry scan and the path accessors agree — no path bypasses spacesDirName.
func TestScope_RoutesThroughSpacesRoot(t *testing.T) {
	s := newScope("nori", "/cache")
	dir := s.spaceDir()
	if got, want := spacesRoot("/cache"), "/cache/"+spacesDirName; got != want {
		t.Errorf("spacesRoot = %q, want %q", got, want)
	}
	for name, got := range map[string]string{
		"runcStateRoot": s.runcStateRoot(),
		"volumesRoot":   s.volumesRoot(),
		"bundleRoot":    s.bundleRoot(),
	} {
		if !strings.HasPrefix(got, dir+"/") {
			t.Errorf("%s = %q is not under spaceDir %q", name, got, dir)
		}
	}
}

// The standalone space (empty name) keeps the legacy flat, unscoped layout so
// the CI single/multi-container path stays byte-identical.
func TestScope_StandaloneLegacyLayout(t *testing.T) {
	s := newScope("", "/cache")
	if got, want := s.bundleRoot(), "/cache/deploy-bundles"; got != want {
		t.Errorf("standalone bundleRoot = %q, want %q", got, want)
	}
	if got, want := s.runcStateRoot(), "/cache/deploy-runc-state"; got != want {
		t.Errorf("standalone runcStateRoot = %q, want %q", got, want)
	}
}
