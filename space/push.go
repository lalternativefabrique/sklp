package space

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// PushOCILayout pushes an OCI image layout (the dir ExportImage wrote) to ref,
// using the SAME containerd resolver the pull path uses (auth from
// the sklp registries file, plain-HTTP for localhost). It pushes the layers and
// config first, then the manifest last (a registry validates a manifest against
// blobs it already holds). manifestDesc identifies the image manifest blob in
// the layout. No daemon — this is the daemonless replacement for `docker push`.
func PushOCILayout(ctx context.Context, layoutDir string, manifestDesc ocispec.Descriptor, ref string) error {
	ref, err := normalizeRef(ref)
	if err != nil {
		return err
	}
	resolver := newImageResolver(ctx)
	pusher, err := resolver.Pusher(ctx, ref)
	if err != nil {
		return fmt.Errorf("pusher for %s: %w", ref, err)
	}

	// Read the manifest to learn its config + layer descriptors.
	manifestBytes, err := readLayoutBlob(layoutDir, manifestDesc.Digest.Encoded())
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return fmt.Errorf("decode manifest: %w", err)
	}

	// Push blobs the manifest references (config + layers) BEFORE the manifest.
	for _, d := range append([]ocispec.Descriptor{manifest.Config}, manifest.Layers...) {
		f, err := os.Open(blobPath(layoutDir, d.Digest.Encoded()))
		if err != nil {
			return fmt.Errorf("open blob %s: %w", d.Digest, err)
		}
		err = pushOne(ctx, pusher, d, f)
		f.Close()
		if err != nil {
			return fmt.Errorf("push blob %s: %w", d.Digest, err)
		}
	}
	// Finally the manifest itself.
	if err := pushOne(ctx, pusher, manifestDesc, bytes.NewReader(manifestBytes)); err != nil {
		return fmt.Errorf("push manifest: %w", err)
	}
	return nil
}

// pushOne pushes one blob through the resolver's content.Writer. A blob the
// registry already holds returns ErrAlreadyExists from Push — treated as
// success (idempotent re-push). Commit seals it with the expected digest+size.
func pushOne(ctx context.Context, pusher remotesPusher, desc ocispec.Descriptor, r io.Reader) error {
	w, err := pusher.Push(ctx, desc)
	if err != nil {
		if errdefs.IsAlreadyExists(err) {
			return nil
		}
		return err
	}
	defer w.Close()
	if _, err := io.Copy(w, r); err != nil {
		return err
	}
	if err := w.Commit(ctx, desc.Size, desc.Digest); err != nil {
		if errdefs.IsAlreadyExists(err) {
			return nil
		}
		return err
	}
	return nil
}

// remotesPusher is the minimal Push surface we need (avoids importing the whole
// remotes interface set here).
type remotesPusher interface {
	Push(ctx context.Context, d ocispec.Descriptor) (content.Writer, error)
}

func readLayoutBlob(layoutDir, encoded string) ([]byte, error) {
	return os.ReadFile(blobPath(layoutDir, encoded))
}

func blobPath(layoutDir, encoded string) string {
	return filepath.Join(layoutDir, "blobs", "sha256", encoded)
}
