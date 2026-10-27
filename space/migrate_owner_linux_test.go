//go:build linux

package space

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/lalternativefabrique/sklp/space/ociproc"
	"github.com/lalternativefabrique/sklp/space/spacenet"
)

func TestMigrateVolumeOwner_RoundTrip(t *testing.T) {
	id, err := ociproc.ResolveIdentity(os.Getuid(), os.Getgid())
	if err != nil || id.UIDCount < 2 {
		t.Skipf("no delegated range: %v", err)
	}
	if _, err := os.Stat("/usr/bin/newuidmap"); err != nil {
		t.Skip("newuidmap unavailable")
	}
	home := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("SKLP_DATA_ROOT", filepath.Join(home, "data"))
	sp, err := NewSpace("mig")
	if err != nil {
		t.Skipf("NewSpace: %v", err)
	}
	t.Cleanup(func() { _ = spacenet.RemoveAllAsRoot(filepath.Join(home, "data")) })
	vol := filepath.Join(sp.volumesRoot(), "pgdata")
	if err := os.MkdirAll(vol, 0o700); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(vol, "PG_VERSION")
	if err := os.WriteFile(f, []byte("16\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	moved, err := sp.MigrateVolumeOwner(false)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if len(moved) != 1 || moved[0] != "pgdata" {
		t.Fatalf("moved = %v", moved)
	}
	slot, _ := childRootSlot(sp.cacheRoot, "mig")
	spaceRoot := id.UIDBase + (id.UIDCount - slot) - 1
	// The volume is the space root's now, 0700: the operator sees the dir, not
	// what is inside, which is the point.
	st, err := os.Lstat(vol)
	if err != nil {
		t.Fatal(err)
	}
	if got := int(st.Sys().(*syscall.Stat_t).Uid); got != spaceRoot {
		t.Fatalf("%s owned by %d, want the space root %d", vol, got, spaceRoot)
	}
	if _, err := os.Lstat(f); err == nil {
		t.Fatalf("the operator can still look inside the migrated volume")
	}
	if _, err := sp.MigrateVolumeOwner(true); err != nil {
		t.Fatalf("back: %v", err)
	}
	st, err = os.Lstat(f)
	if err != nil {
		t.Fatalf("back: %v", err)
	}
	if got := int(st.Sys().(*syscall.Stat_t).Uid); got != os.Getuid() {
		t.Fatalf("back: owned by %d, want the operator", got)
	}
}
