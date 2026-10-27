//go:build linux

package imagebuild

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lalternativefabrique/sklp/space/platform"
	"github.com/lalternativefabrique/sklp/space/runnerbuild"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Image is the produced OCI artifact: the layout dir on disk plus the manifest
// descriptor (digest + size) the pusher needs.
type Image struct {
	LayoutDir  string             // an OCI image layout (oci-layout + index.json + blobs/)
	Manifest   ocispec.Descriptor // the image manifest descriptor
	DiffID     digest.Digest      // the single layer's uncompressed digest (config rootfs)
	ConfigJSON []byte             // the image config (for inspection/tests)
}

// ExportImage turns a built final rootfs into a single-layer OCI image written
// as an image layout under layoutDir. One layer keeps it simple and valid: the
// whole final rootfs is the layer (tar), gzip-compressed; its config carries the
// ENV/WORKDIR/USER/ENTRYPOINT/CMD/EXPOSE gathered during the build. The result
// is a standard OCI layout a registry pusher (or `oci:` consumer) accepts.
//
// A single squashed layer is intentional: the project images are small runtime
// images and we don't need BuildKit's per-instruction layer cache here — the
// recipe-hash cache covers rebuild avoidance upstream.
func ExportImage(ctx context.Context, res Result, layoutDir string) (Image, error) {
	target := res.Platform
	if target.IsZero() {
		target = platform.Host()
	}
	blobs := filepath.Join(layoutDir, "blobs", "sha256")
	if err := os.MkdirAll(blobs, 0o755); err != nil {
		return Image{}, err
	}

	// 1. Layer: tar the whole final rootfs, gzip it, capturing BOTH the
	//    compressed digest (the layer blob name) and the uncompressed digest
	//    (the diff_id the config references).
	layerDesc, diffID, err := writeLayerBlob(ctx, res.Rootfs, blobs)
	if err != nil {
		return Image{}, fmt.Errorf("write layer: %w", err)
	}

	// 2. Config: the OCI image config JSON, referencing the single diff_id.
	cfg := buildImageConfig(res, target, diffID)
	configJSON, err := json.Marshal(cfg)
	if err != nil {
		return Image{}, err
	}
	configDesc, err := writeBlob(blobs, configJSON, ocispec.MediaTypeImageConfig)
	if err != nil {
		return Image{}, err
	}

	// 3. Manifest: ties config + layer together.
	manifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    []ocispec.Descriptor{layerDesc},
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return Image{}, err
	}
	manifestDesc, err := writeBlob(blobs, manifestJSON, ocispec.MediaTypeImageManifest)
	if err != nil {
		return Image{}, err
	}
	manifestDesc.Platform = &ocispec.Platform{OS: target.OS, Architecture: target.Arch}

	// 4. Layout marker + index.json pointing at the manifest.
	if err := os.WriteFile(filepath.Join(layoutDir, "oci-layout"),
		[]byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); err != nil {
		return Image{}, err
	}
	index := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{manifestDesc},
	}
	indexJSON, err := json.Marshal(index)
	if err != nil {
		return Image{}, err
	}
	if err := os.WriteFile(filepath.Join(layoutDir, "index.json"), indexJSON, 0o644); err != nil {
		return Image{}, err
	}

	return Image{
		LayoutDir:  layoutDir,
		Manifest:   manifestDesc,
		DiffID:     diffID,
		ConfigJSON: configJSON,
	}, nil
}

// writeLayerBlob tars the rootfs, gzips it into the blob store, and returns the
// layer descriptor (compressed digest+size, mediatype) plus the uncompressed
// (diff_id) digest the config needs. It streams through a temp file so the
// content-addressed name is known only after hashing, then renames into place.
//
// The tar runs inside a user namespace with the same uid mapping the RUN steps
// used, so files chowned to mapped subuid ranges (e.g. apt cache dirs written
// as container-root) are readable.
func writeLayerBlob(ctx context.Context, rootfs, blobs string) (ocispec.Descriptor, digest.Digest, error) {
	tmp, err := os.CreateTemp(blobs, "layer-*.tmp")
	if err != nil {
		return ocispec.Descriptor{}, "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	// Hash the COMPRESSED stream (blob name) and the UNCOMPRESSED tar (diff_id)
	// simultaneously: tar -> [diffDigester] -> gzip -> [compDigester+file].
	compDigester := sha256.New()
	gzw := gzip.NewWriter(io.MultiWriter(tmp, compDigester))
	diffDigester := sha256.New()
	tarDest := io.MultiWriter(gzw, diffDigester)

	if err := tarRootfsInUserns(ctx, rootfs, tarDest); err != nil {
		tmp.Close()
		return ocispec.Descriptor{}, "", fmt.Errorf("tar rootfs: %w", err)
	}
	if err := gzw.Close(); err != nil {
		tmp.Close()
		return ocispec.Descriptor{}, "", err
	}
	if err := tmp.Close(); err != nil {
		return ocispec.Descriptor{}, "", err
	}

	compDigest := digest.NewDigest(digest.SHA256, compDigester)
	diffID := digest.NewDigest(digest.SHA256, diffDigester)
	fi, err := os.Stat(tmpName)
	if err != nil {
		return ocispec.Descriptor{}, "", err
	}
	final := filepath.Join(blobs, compDigest.Encoded())
	if err := os.Rename(tmpName, final); err != nil {
		return ocispec.Descriptor{}, "", err
	}
	return ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayerGzip,
		Digest:    compDigest,
		Size:      fi.Size(),
	}, diffID, nil
}

// tarRootfsInUserns streams a POSIX tar of rootfs to dst from inside a user
// namespace that matches the uid mapping the RUN steps used. This is required
// because RUN steps run as uid 0 inside the build userns, which maps to subuid
// ranges on the host filesystem — apt and dpkg chown their cache dirs to those
// mapped uids (e.g. /var/cache/apt/archives/partial, 0700 owned by 100041) and
// a tar outside that mapping cannot read them.
//
// The mapping is read from /proc/self/uid_map, not guessed from the build uid.
// Keying it on os.Getuid()!=0 broke every publish on the CI nodes: those pods
// are privileged and run as real root in the host user namespace, so the build
// uid is 0 while the mapping is the identity one. The subuid range was then
// omitted and the export died with
// "tar: ./var/cache/apt/archives/partial: Cannot open: Permission denied".
//
// `tar -c --numeric-owner` produces OCI-compatible archives: numeric uid/gid,
// no local username resolution.
func tarRootfsInUserns(ctx context.Context, rootfs string, dst io.Writer) error {
	var cmd *exec.Cmd
	if runnerbuild.NestedUserns() {
		// Already root against the build's mapping (a Space CI runner): a nested
		// child uid_map may only name uids the parent maps, and unshare --user
		// there is refused outright, even for --map-root-user's identity map.
		cmd = exec.CommandContext(ctx, "tar", "-c", "--numeric-owner", "-C", rootfs, ".")
	} else {
		args := []string{"--user", "--map-root-user",
			"--map-users=1:100000:65535", "--map-groups=1:100000:65535",
			"--mount", "--pid", "--fork",
			"tar", "-c", "--numeric-owner", "-C", rootfs, "."}
		cmd = exec.CommandContext(ctx, "unshare", args...)
	}
	cmd.Stdout = dst
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		if errBuf.Len() > 0 {
			return fmt.Errorf("%w: %s", err, errBuf.String())
		}
		return err
	}
	return nil
}

// writeBlob content-addresses bytes into the blob store and returns a descriptor.
func writeBlob(blobs string, data []byte, mediaType string) (ocispec.Descriptor, error) {
	d := digest.FromBytes(data)
	if err := os.WriteFile(filepath.Join(blobs, d.Encoded()), data, 0o644); err != nil {
		return ocispec.Descriptor{}, err
	}
	return ocispec.Descriptor{MediaType: mediaType, Digest: d, Size: int64(len(data))}, nil
}

// buildCreatedTime resolves the image config .created timestamp. It honours
// SOURCE_DATE_EPOCH (Unix seconds) for reproducible builds; otherwise it uses
// the real build time. Never leave the time.Time zero value: it serialises to
// 0001-01-01T00:00:00Z, which makes ArgoCD Image Updater (newest-build) treat
// every pushed image as older than what's deployed and skip it forever.
func buildCreatedTime() time.Time {
	if v := os.Getenv("SOURCE_DATE_EPOCH"); v != "" {
		if sec, err := strconv.ParseInt(v, 10, 64); err == nil {
			return time.Unix(sec, 0).UTC()
		}
	}
	return time.Now().UTC()
}

// buildImageConfig assembles the OCI image config from the build result + the
// single layer's diff_id.
func buildImageConfig(res Result, target platform.Platform, diffID digest.Digest) ocispec.Image {
	exposed := map[string]struct{}{}
	for _, p := range res.Exposed {
		exposed[p+"/tcp"] = struct{}{}
	}
	img := ocispec.Image{
		Platform: ocispec.Platform{OS: target.OS, Architecture: target.Arch},
		Config: ocispec.ImageConfig{
			Env:          res.Env,
			Entrypoint:   res.Entrypoint,
			Cmd:          res.Cmd,
			WorkingDir:   res.Workdir,
			User:         res.User,
			ExposedPorts: exposed,
		},
		RootFS: ocispec.RootFS{
			Type:    "layers",
			DiffIDs: []digest.Digest{diffID},
		},
	}
	created := buildCreatedTime()
	img.Created = &created
	return img
}
