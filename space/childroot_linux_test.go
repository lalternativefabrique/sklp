//go:build linux

package space

import "testing"

func TestChildRootSlot_StablePerSpaceAndDistinctAcrossSpaces(t *testing.T) {
	root := t.TempDir()
	a, err := childRootSlot(root, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	b, err := childRootSlot(root, "beta")
	if err != nil {
		t.Fatal(err)
	}
	again, err := childRootSlot(root, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("two spaces got slot %d", a)
	}
	if again != a {
		t.Fatalf("alpha moved from slot %d to %d", a, again)
	}
	if _, err := childRootSlot(root, ""); err == nil {
		t.Fatal("a standalone space must not get a slot")
	}
}
