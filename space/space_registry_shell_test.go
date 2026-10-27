package space

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveEmptyShellsIn(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, "spaces", rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mk("ghost/runc-state")
	mk("kept/runc-state")
	mk("live/runc-state")
	if err := os.WriteFile(filepath.Join(root, "spaces", "live", "runc-state", "state.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	removed, err := removeEmptyShellsIn(root, []string{"kept"})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "ghost" {
		t.Fatalf("removed = %v, want [ghost]", removed)
	}
	for name, want := range map[string]bool{"ghost": false, "kept": true, "live": true} {
		_, serr := os.Stat(filepath.Join(root, "spaces", name))
		if exists := serr == nil; exists != want {
			t.Errorf("%s exists = %v, want %v", name, exists, want)
		}
	}
}
