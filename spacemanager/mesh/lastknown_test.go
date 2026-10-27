package mesh

import (
	"testing"
)

func TestLastKnownRoundTrips(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	peers := []Peer{
		{ID: "a", PubKey: "pk-a", PrivateIP: "10.90.0.2", Endpoint: "10.70.0.10:51820"},
		{ID: "b", PubKey: "pk-b", PrivateIP: "10.90.0.3"},
	}
	if err := saveLastKnown("10.90.0.2", peers); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, ok := loadLastKnown()
	if !ok {
		t.Fatal("nothing loaded back")
	}
	if got.SelfMeshIP != "10.90.0.2" || len(got.Peers) != 2 {
		t.Fatalf("round trip lost data: %+v", got)
	}
	if got.Peers[0].PubKey != "pk-a" || got.Peers[1].PrivateIP != "10.90.0.3" {
		t.Fatalf("peer fields lost: %+v", got.Peers)
	}
}

// A node that has never joined has nothing to fall back on: Join must report
// the real registration error rather than bring up an empty wg0.
func TestLastKnownIsAbsentBeforeAnyJoin(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, ok := loadLastKnown(); ok {
		t.Fatal("reported a cache where none was written")
	}
}

func TestLastKnownRejectsAnUnusableCache(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// A file without the node's own mesh IP cannot bring wg0 up.
	if err := saveLastKnown("", []Peer{{ID: "a"}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, ok := loadLastKnown(); ok {
		t.Fatal("accepted a cache with no self mesh IP")
	}
}

// The cache sits beside the identity it describes, not in ~/.cache: `sklp
// clean` empties that, and a wiped cache would silently remove the fallback.
func TestLastKnownLivesBesideTheIdentity(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if dir := meshConfigDir(); lastKnownPath() != dir+"/peers.json" {
		t.Fatalf("cache path %q is not in the mesh identity dir %q", lastKnownPath(), dir)
	}
}
