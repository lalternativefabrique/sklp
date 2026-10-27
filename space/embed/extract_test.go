package embed

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExtractWritesExecutable(t *testing.T) {
	cache := t.TempDir()
	content := []byte("#!/bin/sh\necho hi\n")

	path, err := extract(cache, "runc", sumHex(content), content)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	want := filepath.Join(cache, "runtime", "bin", "runc")
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("extracted file is not executable: mode %v", info.Mode())
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(content) {
		t.Fatalf("content mismatch: got %q", got)
	}
}

func TestExtractIdempotentDoesNotRewrite(t *testing.T) {
	cache := t.TempDir()
	content := []byte("binary-bytes")

	path, err := extract(cache, "slirp4netns", sumHex(content), content)
	if err != nil {
		t.Fatalf("first extract: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// Same content → must not rewrite (mtime preserved).
	path2, err := extract(cache, "slirp4netns", sumHex(content), content)
	if err != nil {
		t.Fatalf("second extract: %v", err)
	}
	if path2 != path {
		t.Fatalf("path changed: %q vs %q", path2, path)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("file was rewritten: mtime %v -> %v", before.ModTime(), after.ModTime())
	}
}

func TestExtractRewritesStaleContent(t *testing.T) {
	cache := t.TempDir()

	oldContent := []byte("old-version")
	path, err := extract(cache, "runc", sumHex(oldContent), oldContent)
	if err != nil {
		t.Fatalf("extract old: %v", err)
	}

	newContent := []byte("new-version-after-sklp-upgrade")
	if _, err := extract(cache, "runc", sumHex(newContent), newContent); err != nil {
		t.Fatalf("extract new: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(newContent) {
		t.Fatalf("stale content not rewritten: got %q", got)
	}
	if sumHex(got) != sumHex(newContent) {
		t.Fatalf("hash mismatch after rewrite")
	}
}

func TestExtractRejectsEmpty(t *testing.T) {
	cache := t.TempDir()
	_, err := extract(cache, "runc", sumHex(nil), nil)
	if err == nil {
		t.Fatal("expected error for empty embedded binary, got nil")
	}
}

func TestExtractRejectsUnexpectedHash(t *testing.T) {
	cache := t.TempDir()
	_, err := extract(cache, "runc", sumHex([]byte("expected")), []byte("tampered"))
	if err == nil {
		t.Fatal("expected hash mismatch for tampered embedded binary")
	}
}
