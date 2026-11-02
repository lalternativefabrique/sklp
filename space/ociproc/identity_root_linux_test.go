//go:build linux

package ociproc

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveIdentityRootUsesItsSubIDRange(t *testing.T) {
	if mappedRangeAboveRoot(uidMapPath) > 0 {
		t.Skip("inside a user namespace that maps a range: root takes that range instead")
	}
	dir := t.TempDir()
	oldUIDPath, oldGIDPath := subuidPath, subgidPath
	subuidPath = filepath.Join(dir, "subuid")
	subgidPath = filepath.Join(dir, "subgid")
	t.Cleanup(func() {
		subuidPath = oldUIDPath
		subgidPath = oldGIDPath
	})
	if err := os.WriteFile(subuidPath, []byte("root:100000:65536\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(subgidPath, []byte("root:100000:65536\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := ResolveIdentity(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if id.UIDBase != 100000 || id.UIDCount != 65536 || id.GIDBase != 100000 {
		t.Fatalf("identity = %+v, want root's delegated range", id)
	}
	uid, _ := id.Mappings()
	if len(uid) != 2 || uid[1]["hostID"] != 100000 {
		t.Fatalf("mappings = %v, want 1..N on the subuid range", uid)
	}
}

func TestResolveIdentityRootWithoutRangeStaysSingle(t *testing.T) {
	if mappedRangeAboveRoot(uidMapPath) > 0 {
		t.Skip("inside a user namespace that maps a range")
	}
	dir := t.TempDir()
	oldUIDPath, oldGIDPath := subuidPath, subgidPath
	subuidPath = filepath.Join(dir, "subuid")
	subgidPath = filepath.Join(dir, "subgid")
	t.Cleanup(func() {
		subuidPath = oldUIDPath
		subgidPath = oldGIDPath
	})
	id, err := ResolveIdentity(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if uid, _ := id.Mappings(); len(uid) != 1 {
		t.Fatalf("mappings = %v, want the single-id mapping", uid)
	}
}
