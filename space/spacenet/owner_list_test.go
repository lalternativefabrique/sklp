package spacenet

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

func TestApplyOwnerList_ChownsListedPathsAndSkipsGoneOnes(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	me, mg := os.Getuid(), os.Getgid()
	list := filepath.Join(t.TempDir(), "owners")
	body := []byte(itoa(me) + " " + itoa(mg) + " a/b\n" + itoa(me) + " " + itoa(mg) + " gone/by/whiteout\n")
	if err := os.WriteFile(list, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := applyOwnerList(root, list); err != nil {
		t.Fatalf("applyOwnerList: %v", err)
	}
	st, _ := os.Lstat(f)
	if sys := st.Sys().(*syscall.Stat_t); int(sys.Uid) != me {
		t.Fatalf("owner = %d, want %d", sys.Uid, me)
	}
	if err := os.WriteFile(list, []byte("x y a/b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := applyOwnerList(root, list); err == nil {
		t.Fatal("a malformed line was accepted")
	}
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
