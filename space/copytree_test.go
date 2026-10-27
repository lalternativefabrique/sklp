package space

import (
	"os"
	"path/filepath"
	"testing"
)

// A rootless build chowns dirs to subuids this process is not, leaving them
// unreadable — /var/lib/pgbackrest is the case that surfaced it. A dir with no
// u+x reproduces the same walk failure without needing a uid map.
func TestCopyTreeCopiesUntraversableDir(t *testing.T) {
	src := t.TempDir()
	deep := filepath.Join(src, "var", "lib", "pgbackrest")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "keep"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(src, "var", "lib")
	if err := os.Chmod(locked, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	// The escalation forks self into a userns, which the test binary cannot do
	// (it never runs MaybeRunSubidRemove). Ownership is not what this case is
	// about — the dir is ours — so stub it to the plain path.
	orig := subidChownTree
	t.Cleanup(func() { subidChownTree = orig })
	subidChownTree = func(string) error { return nil }

	dst := filepath.Join(t.TempDir(), "out")
	if err := copyTree(src, dst); err != nil {
		t.Fatalf("copyTree: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "var", "lib", "pgbackrest", "keep"))
	if err != nil {
		t.Fatalf("file under an untraversable dir was not copied: %v", err)
	}
	if string(got) != "data" {
		t.Fatalf("content: got %q", got)
	}
}
