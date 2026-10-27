package space

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// mustLayerBlob writes a gzipped tar holding one regular file, as the pull path
// downloads it into blobs/.
func mustLayerBlob(t *testing.T, path, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(content)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
}

// A layer applied successfully leaves its content in the rootfs and no tarball
// behind. Keeping the archive was what filled a production disk: it is never
// read again, since a cached image returns on the .extracted sentinel before
// blobs/ is consulted.
func TestApplyLayerBlob_RemovesSpentTarball(t *testing.T) {
	root := t.TempDir()
	rootfs := filepath.Join(root, "rootfs", "deadbeef")
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(root, "blobs", "cafebabe")
	mustLayerBlob(t, blob, "etc/motd", "hello")

	if err := applyLayerBlob(context.Background(), rootfs, blob, nil); err != nil {
		t.Fatalf("applyLayerBlob: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(rootfs, "etc", "motd"))
	if err != nil {
		t.Fatalf("layer content missing from rootfs: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("rootfs content = %q, want %q", got, "hello")
	}
	if _, err := os.Stat(blob); !os.IsNotExist(err) {
		t.Errorf("spent tarball still present (err=%v), want removed", err)
	}
}

// A layer that fails to apply keeps its tarball: removal is tied to a
// successful extraction, so a retry does not have to re-download.
func TestApplyLayerBlob_KeepsTarballOnFailure(t *testing.T) {
	root := t.TempDir()
	blob := filepath.Join(root, "blobs", "notgzip")
	if err := os.MkdirAll(filepath.Dir(blob), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blob, []byte("not a gzip stream"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := applyLayerBlob(context.Background(), filepath.Join(root, "rootfs"), blob, nil); err == nil {
		t.Fatal("applyLayerBlob succeeded on a corrupt blob, want error")
	}
	if _, err := os.Stat(blob); err != nil {
		t.Errorf("tarball removed despite a failed extraction: %v", err)
	}
}
