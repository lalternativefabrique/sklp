package space

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func writeOwnedLayer(t *testing.T, path string, entries []tar.Header) {
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
	for _, h := range entries {
		h := h
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestApplyLayerBlob_RecordsDeclaredOwnersLastLayerWins(t *testing.T) {
	root := t.TempDir()
	rootfs := filepath.Join(root, "rootfs")
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		t.Fatal(err)
	}
	owners := layerOwners{}
	first := filepath.Join(root, "blobs", "one")
	writeOwnedLayer(t, first, []tar.Header{
		{Name: "var/www/html/bootstrap/cache/", Typeflag: tar.TypeDir, Mode: 0o755, Uid: 33, Gid: 33},
		{Name: "etc/app.conf", Typeflag: tar.TypeReg, Mode: 0o644, Uid: 33, Gid: 33},
	})
	if err := applyLayerBlob(context.Background(), rootfs, first, owners); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(root, "blobs", "two")
	writeOwnedLayer(t, second, []tar.Header{
		{Name: "etc/app.conf", Typeflag: tar.TypeReg, Mode: 0o644},
	})
	if err := applyLayerBlob(context.Background(), rootfs, second, owners); err != nil {
		t.Fatal(err)
	}
	if got := owners["var/www/html/bootstrap/cache"]; got != [2]int{33, 33} {
		t.Fatalf("bootstrap/cache owner = %v, want 33:33", got)
	}
	if got := owners["etc/app.conf"]; got != [2]int{0, 0} {
		t.Fatalf("a path rewritten by a later layer takes that layer's owner: got %v", got)
	}
}
