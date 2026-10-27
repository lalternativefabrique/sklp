package space

import (
	"archive/tar"
	"context"
	"runtime"
	"testing"

	"github.com/containerd/containerd/v2/core/remotes/docker"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestPickLinuxNative_SelectsMatchingPlatform(t *testing.T) {
	other := "amd64"
	if runtime.GOARCH == "amd64" {
		other = "arm64"
	}
	index := ocispec.Index{
		Manifests: []ocispec.Descriptor{
			{Digest: "sha256:other", Platform: &ocispec.Platform{OS: "linux", Architecture: other}},
			{Digest: "sha256:native", Platform: &ocispec.Platform{OS: "linux", Architecture: runtime.GOARCH}},
			{Digest: "sha256:win", Platform: &ocispec.Platform{OS: "windows", Architecture: runtime.GOARCH}},
		},
	}
	got, err := pickLinux(index, "example/image:tag", runtime.GOARCH)
	if err != nil {
		t.Fatalf("pickLinux: %v", err)
	}
	if got.Digest != "sha256:native" {
		t.Errorf("picked digest = %q, want sha256:native", got.Digest)
	}
}

func TestPickLinuxNative_NoMatchErrors(t *testing.T) {
	other := "amd64"
	if runtime.GOARCH == "amd64" {
		other = "arm64"
	}
	index := ocispec.Index{
		Manifests: []ocispec.Descriptor{
			{Digest: "sha256:other", Platform: &ocispec.Platform{OS: "linux", Architecture: other}},
		},
	}
	if _, err := pickLinux(index, "example/image:tag", runtime.GOARCH); err == nil {
		t.Fatal("expected error when no native linux manifest is present")
	}
}

func TestPickLinuxNative_IgnoresNilPlatform(t *testing.T) {
	index := ocispec.Index{
		Manifests: []ocispec.Descriptor{
			{Digest: "sha256:nilplat", Platform: nil},
			{Digest: "sha256:native", Platform: &ocispec.Platform{OS: "linux", Architecture: runtime.GOARCH}},
		},
	}
	got, err := pickLinux(index, "example/image:tag", runtime.GOARCH)
	if err != nil {
		t.Fatalf("pickLinux: %v", err)
	}
	if got.Digest != "sha256:native" {
		t.Errorf("picked digest = %q, want sha256:native", got.Digest)
	}
}

// TestNewImageResolver_LocalRegistryIsPlainHTTP documents the dev-image bridge:
// the resolver must treat a local registry as plain HTTP (a dev-built image is
// pushed to a localhost registry serving HTTP), while remote registries stay
// HTTPS. We assert the predicate the resolver is built with (docker's
// MatchLocalhost), which is the contract newImageResolver relies on.
func TestNewImageResolver_LocalRegistryIsPlainHTTP(t *testing.T) {
	// Building the resolver must not panic and must return a usable value.
	if newImageResolver(context.Background()) == nil {
		t.Fatal("newImageResolver returned nil")
	}
	cases := map[string]bool{
		"localhost:5000":       true,  // local dev registry → HTTP
		"127.0.0.1:5000":       true,  // local dev registry → HTTP
		"registry.example.com": false, // remote → HTTPS
		"rg.fr-par.scw.cloud":  false, // remote → HTTPS
	}
	for host, wantHTTP := range cases {
		got, _ := docker.MatchLocalhost(host)
		if got != wantHTTP {
			t.Errorf("MatchLocalhost(%q) = %v, want %v (local→HTTP, remote→HTTPS)", host, got, wantHTTP)
		}
	}
}

// TestRootlessLayerFilter_WritableDirs adds the owner write bit to directories
// (so a rootless extraction can populate a read-only dir like a baked Go module
// cache), and leaves non-directory entries untouched. It always keeps the entry.
func TestRootlessLayerFilter_WritableDirs(t *testing.T) {
	// A read-only directory gets u+w added.
	dir := &tar.Header{Typeflag: tar.TypeDir, Mode: 0o555}
	keep, err := rootlessLayerFilter(dir)
	if err != nil || !keep {
		t.Fatalf("rootlessLayerFilter(dir) = (%v, %v), want (true, nil)", keep, err)
	}
	if dir.Mode&0o200 == 0 {
		t.Errorf("directory mode = %o, want owner-write bit set", dir.Mode)
	}
	// A regular file is left as-is.
	file := &tar.Header{Typeflag: tar.TypeReg, Mode: 0o444}
	if _, _ = rootlessLayerFilter(file); file.Mode != 0o444 {
		t.Errorf("file mode changed to %o, want it untouched (0444)", file.Mode)
	}
}

// A 0000 file is what the shadow package bakes for /etc/gshadow. Extracting
// it is fine; a later layer rewriting that path fails, because the extractor
// opens the existing file for writing and 0000 denies even its owner. This is
// what "apply layer …: /etc/gshadow: permission denied" was.
func TestRootlessLayerFilter_UnreadableFileGetsOwnerAccess(t *testing.T) {
	file := &tar.Header{Typeflag: tar.TypeReg, Mode: 0}
	keep, err := rootlessLayerFilter(file)
	if err != nil || !keep {
		t.Fatalf("rootlessLayerFilter(0000 file) = (%v, %v), want (true, nil)", keep, err)
	}
	if file.Mode&0o600 != 0o600 {
		t.Errorf("mode = %o, want the owner able to read and write it", file.Mode)
	}
}

// TestRootlessLayerFilter_DropsSpecialFiles drops char/block/fifo entries: a
// rootless userns cannot mknod them, so keeping them would abort the layer with
// EPERM. The runtime rebuilds /dev at create time, so dropping them is safe.
func TestRootlessLayerFilter_DropsSpecialFiles(t *testing.T) {
	for _, tf := range []byte{tar.TypeChar, tar.TypeBlock, tar.TypeFifo} {
		keep, err := rootlessLayerFilter(&tar.Header{Typeflag: tf, Name: "dev/x"})
		if err != nil {
			t.Errorf("rootlessLayerFilter(type %d) err = %v, want nil", tf, err)
		}
		if keep {
			t.Errorf("rootlessLayerFilter(type %d) kept the entry, want it dropped", tf)
		}
	}
}

// TestRootlessLayerFilter_StripsPrivilegedXattrs removes security.*/trusted.*
// xattrs (e.g. caddy's security.capability = cap_net_bind_service) that a
// rootless extraction cannot setxattr without CAP_SYS_ADMIN in the initial
// userns — otherwise the whole layer aborts with EPERM. user.* xattrs are kept.
func TestRootlessLayerFilter_StripsPrivilegedXattrs(t *testing.T) {
	h := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     "usr/bin/caddy",
		PAXRecords: map[string]string{
			"SCHILY.xattr.security.capability": "\x01\x00\x00\x02...",
			"SCHILY.xattr.trusted.foo":         "bar",
			"SCHILY.xattr.user.note":           "keepme",
			"mtime":                            "1700000000", // non-xattr PAX record, untouched
		},
	}
	keep, err := rootlessLayerFilter(h)
	if err != nil || !keep {
		t.Fatalf("rootlessLayerFilter = (%v, %v), want (true, nil)", keep, err)
	}
	if _, ok := h.PAXRecords["SCHILY.xattr.security.capability"]; ok {
		t.Error("security.capability xattr was kept, want it stripped")
	}
	if _, ok := h.PAXRecords["SCHILY.xattr.trusted.foo"]; ok {
		t.Error("trusted.* xattr was kept, want it stripped")
	}
	if v := h.PAXRecords["SCHILY.xattr.user.note"]; v != "keepme" {
		t.Errorf("user.* xattr = %q, want it preserved (%q)", v, "keepme")
	}
	if v := h.PAXRecords["mtime"]; v != "1700000000" {
		t.Errorf("non-xattr PAX record mtime = %q, want it untouched", v)
	}
}
