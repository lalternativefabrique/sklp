package control

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestListSpaces verifies the multi-space discovery `sklp deploy ps` fans out
// over: only LIVE sockets (one with a real listener) are returned, stale .sock
// nodes (no listener) and non-socket files are skipped, and names are the
// space (without the .sock suffix), sorted.
func TestListSpaces(t *testing.T) {
	// Point socketDir() at an isolated tempdir.
	rt := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", rt)
	dir := filepath.Join(rt, "sklp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	// A live space: bind a real unix listener so socketAlive answers.
	liveSock := filepath.Join(dir, "acme.sock")
	ln, err := net.Listen("unix", liveSock)
	if err != nil {
		t.Fatalf("listen live socket: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	// A stale space: a .sock file with nobody listening — must be skipped.
	if err := os.WriteFile(filepath.Join(dir, "dead.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// A non-socket file — must be ignored.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ListSpaces()
	if err != nil {
		t.Fatalf("ListSpaces: %v", err)
	}
	if !slices.Equal(got, []string{"acme"}) {
		t.Errorf("ListSpaces = %v, want [acme] (live only, stale+non-socket skipped)", got)
	}
}

// TestListSpacesMissingDir: no socket dir yet (fresh host) is an empty list, not
// an error.
func TestListSpacesMissingDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir()) // exists but no sklp/ subdir
	got, err := ListSpaces()
	if err != nil {
		t.Fatalf("ListSpaces on fresh host: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListSpaces = %v, want empty on a host with no spaces", got)
	}
}
