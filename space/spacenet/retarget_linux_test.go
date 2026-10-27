//go:build linux

package spacenet

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/lalternativefabrique/sklp/space/ociproc"
)

// A volume moves between the operator (id 0 of the space) and a space root
// (its slot id) file by file, and only the files of the id being moved:
// what a service's own uid wrote stays its own.
func TestRetargetTreeOwner_MovesOneOwnerOnlyAndBack(t *testing.T) {
	if _, ok := lookupSubidTools(); !ok {
		t.Skip("newuidmap/newgidmap unavailable")
	}
	id, err := ociproc.ResolveIdentity(os.Getuid(), os.Getgid())
	if err != nil || id.UIDCount < 2 {
		t.Skipf("no delegated range: %v", err)
	}
	root := t.TempDir()
	t.Cleanup(func() { _ = RemoveAllAsRoot(root) })
	mine := filepath.Join(root, "pg", "postgresql.conf")
	if err := os.MkdirAll(filepath.Dir(mine), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mine, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	theirs := filepath.Join(root, "pg", "base")
	// A file of a service uid (container 999): made by the space root, who may chown into the range.
	if err := os.WriteFile(theirs, []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RetargetTreeOwner(theirs, 0, 999); err != nil {
		t.Fatalf("seed a service-owned file: %v", err)
	}
	serviceUID := id.UIDBase + 999 - 1
	if got := ownerOf(t, theirs); got != serviceUID {
		t.Fatalf("seed: base owned by %d, want %d", got, serviceUID)
	}

	slotRoot := id.UIDCount // slot 0
	if err := RetargetTreeOwner(root, 0, slotRoot); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	spaceRoot := id.UIDBase + slotRoot - 1
	for _, p := range []string{root, filepath.Dir(mine), mine} {
		if got := ownerOf(t, p); got != spaceRoot {
			t.Fatalf("%s owned by %d after migrate, want the space root %d", p, got, spaceRoot)
		}
	}
	if got := ownerOf(t, theirs); got != serviceUID {
		t.Fatalf("the service's file moved to %d; it must stay %d", got, serviceUID)
	}

	if err := RetargetTreeOwner(root, slotRoot, 0); err != nil {
		t.Fatalf("back: %v", err)
	}
	if got := ownerOf(t, mine); got != os.Getuid() {
		t.Fatalf("back: %s owned by %d, want the operator %d", mine, got, os.Getuid())
	}
	if got := ownerOf(t, theirs); got != serviceUID {
		t.Fatalf("back: the service's file moved to %d", got)
	}
}

func ownerOf(t *testing.T, p string) int {
	t.Helper()
	st, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return int(st.Sys().(*syscall.Stat_t).Uid)
}
