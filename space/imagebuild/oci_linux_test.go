//go:build linux

package imagebuild

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// fakeRootfs writes a tiny rootfs tree so WriteDiff has real content to tar.
func fakeRootfs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app", "binary"), []byte("ELF stub"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestExportImage_ValidLayout: a built rootfs exports to a structurally valid
// single-layer OCI image layout — oci-layout marker, index → manifest →
// config+layer, every referenced blob present and content-addressed, config
// diff_id matching the layer's uncompressed digest, and the build config
// (Env/Entrypoint/Workdir) carried.
func TestExportImage_ValidLayout(t *testing.T) {
	res := Result{
		Rootfs:     fakeRootfs(t),
		Env:        []string{"K=v"},
		Workdir:    "/app",
		Entrypoint: []string{"/app/binary"},
		Exposed:    []string{"4000"},
	}
	layout := t.TempDir()
	img, err := ExportImage(context.Background(), res, layout)
	if err != nil {
		t.Fatalf("ExportImage: %v", err)
	}

	// oci-layout marker.
	if _, err := os.Stat(filepath.Join(layout, "oci-layout")); err != nil {
		t.Errorf("oci-layout marker missing: %v", err)
	}

	// index.json -> the manifest.
	var index ocispec.Index
	mustReadJSON(t, filepath.Join(layout, "index.json"), &index)
	if len(index.Manifests) != 1 || index.Manifests[0].Digest != img.Manifest.Digest {
		t.Fatalf("index does not point at the manifest: %+v", index.Manifests)
	}

	// manifest -> config + one layer, both present as blobs.
	var manifest ocispec.Manifest
	mustReadJSON(t, blobFile(layout, img.Manifest.Digest), &manifest)
	if len(manifest.Layers) != 1 {
		t.Fatalf("want 1 layer, got %d", len(manifest.Layers))
	}
	for _, d := range append([]ocispec.Descriptor{manifest.Config}, manifest.Layers...) {
		if _, err := os.Stat(blobFile(layout, d.Digest)); err != nil {
			t.Errorf("referenced blob %s missing: %v", d.Digest, err)
		}
	}

	// config diff_id == img.DiffID == the layer's uncompressed digest, and the
	// build config carried.
	var cfg ocispec.Image
	mustReadJSON(t, blobFile(layout, manifest.Config.Digest), &cfg)
	if len(cfg.RootFS.DiffIDs) != 1 || cfg.RootFS.DiffIDs[0] != img.DiffID {
		t.Errorf("config diff_id %v != %v", cfg.RootFS.DiffIDs, img.DiffID)
	}
	if cfg.Config.WorkingDir != "/app" || len(cfg.Config.Entrypoint) != 1 {
		t.Errorf("config not carried: %+v", cfg.Config)
	}
	if _, ok := cfg.Config.ExposedPorts["4000/tcp"]; !ok {
		t.Errorf("exposed port missing: %v", cfg.Config.ExposedPorts)
	}

	// The layer blob's filename must equal its content digest (content-addressed).
	layerDesc := manifest.Layers[0]
	if got := digestOfFile(t, blobFile(layout, layerDesc.Digest)); got != layerDesc.Digest {
		t.Errorf("layer blob not content-addressed: file=%s desc=%s", got, layerDesc.Digest)
	}
}

func mustReadJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

func blobFile(layout string, d digest.Digest) string {
	return filepath.Join(layout, "blobs", "sha256", d.Encoded())
}

func digestOfFile(t *testing.T, path string) digest.Digest {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return digest.FromBytes(b)
}
