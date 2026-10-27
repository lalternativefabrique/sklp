//go:build linux

package spacenet

import (
	"os"
	"path/filepath"
	"testing"
)

// A dir the image left without u+x cannot be listed until it is chowned, so the
// descent has to take ownership on the way down rather than list first.
func TestChownTreeDescendsIntoUntraversableDir(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "var", "lib", "pgbackrest")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(deep, "f")
	if err := os.WriteFile(leaf, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(root, "var", "lib")
	if err := os.Chmod(locked, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	if err := chownTree(root, os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("chownTree: %v", err)
	}
	// Reaching the leaf at all is the assertion: the walk had to widen the
	// locked dir to get there.
	if _, err := os.Stat(leaf); err != nil {
		t.Fatalf("leaf under the locked dir was never reached: %v", err)
	}
}

// A symlink must be chowned as the link, never followed: following one would
// hand ownership of whatever it points at, outside the tree.
func TestChownTreeDoesNotFollowSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := chownTree(root, os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("chownTree: %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("symlink target disturbed: %v", err)
	}
}
