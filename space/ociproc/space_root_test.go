//go:build linux

package ociproc

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSpaceRootOf_PoolKeepsEveryImageID(t *testing.T) {
	id := Identity{HostUID: 1001, HostGID: 1001, UIDBase: 165536, GIDBase: 165536, UIDCount: 65536, GIDCount: 65536,
		RootPoolUID: 100000, RootPoolGID: 100000, RootPoolCount: 65536}
	r, ok := id.SpaceRootOf(3, 4096)
	if !ok {
		t.Fatal("slot 3 must fit")
	}
	if r.ChildCount != 65536 {
		t.Fatalf("child maps %d ids, want all 65536 so 65532 and 65534 exist", r.ChildCount)
	}
	if r.HostUID != 100003 || r.HostGID != 100003 || r.WorkerID != 65536+1+3 {
		t.Fatalf("root = %+v, want host 100003 at worker id 65540", r)
	}
	if got := id.WorkerIDOf(r.HostUID); got != r.WorkerID {
		t.Fatalf("WorkerIDOf(%d) = %d, want %d", r.HostUID, got, r.WorkerID)
	}
	if got := id.WorkerIDOf(165536 + 998); got != 999 {
		t.Fatalf("an image id keeps its worker id: got %d, want 999", got)
	}
	if uid, _ := id.HostUIDOf(65532); uid != 165536+65531 {
		t.Fatalf("65532 maps to %d, want it unchanged from the shared model", uid)
	}
}

func TestSpaceRootOf_WithoutPoolCarvesTheTop(t *testing.T) {
	id := Identity{UIDBase: 165536, GIDBase: 165536, UIDCount: 65536, GIDCount: 65536}
	r, ok := id.SpaceRootOf(0, 4096)
	if !ok || r.WorkerID != 65536 || r.HostUID != 165536+65535 || r.ChildCount != 61440 {
		t.Fatalf("root = %+v ok=%v, want the old carve (worker 65536, 61440 ids)", r, ok)
	}
	if _, ok := id.SpaceRootOf(4096, 4096); ok {
		t.Fatal("a slot past the reservation must not fit")
	}
}

func TestSecondSubIDRange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subuid")
	body := "alice:165536:65536\nalice:100000:65536\nother:300000:65536\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, c := secondSubIDRange(path, "alice", 1001, 165536, 65536); b != 100000 || c != 65536 {
		t.Fatalf("second range = %d:%d, want 100000:65536", b, c)
	}
	if err := os.WriteFile(path, []byte("alice:165536:65536\nalice:200000:65536\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, c := secondSubIDRange(path, "alice", 1001, 165536, 65536); c != 0 {
		t.Fatal("an overlapping range must not be taken as the pool")
	}
}
