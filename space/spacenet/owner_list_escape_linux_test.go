package spacenet

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestApplyOwnerListStaysInsideRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "rootfs")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Lchown("link/secret", os.Getuid(), os.Getgid()); !isRootEscape(err) {
		t.Fatalf("os.Root no longer reports an escape the way isRootEscape expects: %v", err)
	}

	list := filepath.Join(base, "owners")
	uid, gid := os.Getuid(), os.Getgid()
	content := fmt.Sprintf("%d %d link/secret\n%d %d etc\n%d %d ../outside/secret\n", uid, gid, uid, gid, uid, gid)
	if err := os.WriteFile(list, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := applyOwnerList(root, list); err != nil {
		t.Fatalf("applyOwnerList: %v", err)
	}
}
