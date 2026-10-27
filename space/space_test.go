package space

import (
	"path/filepath"
	"testing"
)

func TestSpace_Standalone_KeepsLegacyPaths(t *testing.T) {
	s := newScope("", "/cache")
	if !s.standalone() {
		t.Fatal("empty name must be the standalone space")
	}
	if got, want := s.volumesRoot(), filepath.Join("/cache", "volumes"); got != want {
		t.Errorf("volumesRoot = %q, want legacy %q", got, want)
	}
	if got, want := s.runcStateRoot(), filepath.Join("/cache", "deploy-runc-state"); got != want {
		t.Errorf("runcStateRoot = %q, want legacy %q", got, want)
	}
	if got, want := s.containerID("core-0"), deployIDPrefix+"core-0"; got != want {
		t.Errorf("containerID = %q, want legacy %q", got, want)
	}
}

func TestSpace_Named_ScopesEverything(t *testing.T) {
	a := newScope("prod", "/cache")
	b := newScope("staging", "/cache")

	// Volumes are disjoint per space (invariant I2).
	if a.volumesRoot() == b.volumesRoot() {
		t.Error("two spaces must have distinct volumesRoot")
	}
	if got, want := a.volumesRoot(), filepath.Join("/cache", "spaces", "prod", "volumes"); got != want {
		t.Errorf("volumesRoot = %q, want %q", got, want)
	}
	// Container ids are disjoint per space (invariant I4): a "core" in prod
	// and a "core" in staging are different runc ids.
	if a.containerID("core-0") == b.containerID("core-0") {
		t.Error("same-named service in two spaces must have distinct container ids")
	}
	// Runc state roots are disjoint.
	if a.runcStateRoot() == b.runcStateRoot() {
		t.Error("two spaces must have distinct runc state roots")
	}
}

// TestDeployLauncher_VolumesScopedPerSpace proves the launcher materialises a
// same-named volume in DISTINCT dirs for two spaces (invariant I2). It checks
// the path policy only (no runc), so it runs without root.
func TestDeployLauncher_VolumesScopedPerSpace(t *testing.T) {
	a := &Space{scope: newScope("prod", "/cache")}
	b := &Space{scope: newScope("staging", "/cache")}
	if a.volumesRoot() == b.volumesRoot() {
		t.Fatalf("two spaces share a volumesRoot: %q", a.volumesRoot())
	}
	// Standalone keeps the legacy flat dir (back-compat).
	std := &Space{scope: newScope("", "/cache")}
	if std.volumesRoot() != "/cache/volumes" {
		t.Errorf("standalone volumesRoot = %q, want /cache/volumes", std.volumesRoot())
	}
}

// TestSpace_ContainerIDsDisjointAcrossSpaces proves invariant I4: a "core"
// service in two different spaces maps to two different runc ids living in two
// different state roots, so a launch/stop/reap in one never reaches the other.
func TestSpace_ContainerIDsDisjointAcrossSpaces(t *testing.T) {
	a := newScope("prod", "/cache")
	b := newScope("staging", "/cache")
	if a.containerID("core-0") == b.containerID("core-0") {
		t.Errorf("same service in two spaces shares a container id: %q", a.containerID("core-0"))
	}
	if a.runcStateRoot() == b.runcStateRoot() {
		t.Errorf("two spaces share a runc state root: %q", a.runcStateRoot())
	}
	// The id must carry the space name so a state-root scan can filter by it.
	if got := a.containerID("core-0"); got != deployIDPrefix+"prod-core-0" {
		t.Errorf("containerID = %q, want %s", got, deployIDPrefix+"prod-core-0")
	}
}
