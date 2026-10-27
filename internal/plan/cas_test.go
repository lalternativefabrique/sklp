package plan

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCAS_PutThenGet_RoundTrip(t *testing.T) {
	repo := t.TempDir()
	cas := OpenCAS(repo)

	src := filepath.Join(repo, "in.txt")
	content := []byte("hello cas\n")
	if err := os.WriteFile(src, content, 0o600); err != nil {
		t.Fatalf("write src: %v", err)
	}
	digest, err := cas.PutFile(src)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if digest == "" || len(digest) != 64 {
		t.Fatalf("digest looks wrong: %q", digest)
	}

	dst := filepath.Join(repo, "subdir", "out.txt")
	if err := cas.GetTo(digest, dst); err != nil {
		t.Fatalf("get: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("content drift: %q vs %q", got, content)
	}
}

func TestCAS_DedupSameContent(t *testing.T) {
	repo := t.TempDir()
	cas := OpenCAS(repo)

	a := filepath.Join(repo, "a.txt")
	b := filepath.Join(repo, "b.txt")
	content := []byte("identical bytes\n")
	if err := os.WriteFile(a, content, 0o600); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := os.WriteFile(b, content, 0o600); err != nil {
		t.Fatalf("write b: %v", err)
	}

	dA, err := cas.PutFile(a)
	if err != nil {
		t.Fatalf("put a: %v", err)
	}
	dB, err := cas.PutFile(b)
	if err != nil {
		t.Fatalf("put b: %v", err)
	}
	if dA != dB {
		t.Fatalf("identical content produced different digests: %s vs %s", dA, dB)
	}

	// Only one underlying file should exist for the shared digest.
	entry := cas.pathFor(dA)
	info, err := os.Stat(entry)
	if err != nil {
		t.Fatalf("stat dedup entry: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("dedup entry is not a regular file: %v", info.Mode())
	}
}

func TestCAS_GetMissReturnsSentinel(t *testing.T) {
	cas := OpenCAS(t.TempDir())
	err := cas.GetTo("0000000000000000000000000000000000000000000000000000000000000000",
		filepath.Join(t.TempDir(), "out"))
	if !errors.Is(err, ErrCASMiss) {
		t.Fatalf("want ErrCASMiss, got %v", err)
	}
}
