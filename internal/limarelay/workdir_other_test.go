//go:build !linux

package limarelay

import (
	"path/filepath"
	"testing"
)

// The home directory itself is mounted in the guest, so running from it must
// relay like any subdirectory: the prefix test alone rejected it, because
// "/Users/syl" does not start with "/Users/syl/".
func TestWorkdirMounted(t *testing.T) {
	home := filepath.FromSlash("/Users/syl")
	cases := map[string]bool{
		home:                                   true,
		filepath.Join(home, "code", "skalpai"): true,
		filepath.FromSlash("/Users/other"):     false,
		filepath.FromSlash("/tmp"):             false,
	}
	for dir, want := range cases {
		if got := workdirMounted(dir, home); got != want {
			t.Errorf("workdirMounted(%q, %q) = %v, want %v", dir, home, got, want)
		}
	}
}
