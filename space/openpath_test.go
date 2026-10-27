package space

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenPathToOthers_AncestorsBecomeTraversable(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	cache := filepath.Join(home, ".cache", "sklp", "embedded-runtime")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	for p, m := range map[string]os.FileMode{home: 0o750, filepath.Join(home, ".cache"): 0o700, filepath.Join(home, ".cache", "sklp"): 0o700} {
		if err := os.Chmod(p, m); err != nil {
			t.Fatal(err)
		}
	}
	if err := openPathToOthers(cache); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]os.FileMode{home: 0o751, filepath.Join(home, ".cache"): 0o711, filepath.Join(home, ".cache", "sklp"): 0o711, cache: 0o755} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != want {
			t.Errorf("%s: mode %o, want %o", p, st.Mode().Perm(), want)
		}
	}
	if st, _ := os.Stat(root); st.Mode().Perm()&0o011 != 0o011 {
		t.Errorf("the temp root above home should have been opened too: %o", st.Mode().Perm())
	}
}
