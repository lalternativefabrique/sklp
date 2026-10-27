package plan

// Content-addressed store for declared step outputs (engine 2.4 —
// issue #387).
//
// Layout under the repo:
//
//	<repo>/.sklp/cache/cas/<aa>/<sha256-hex>
//
// where <aa> is the first two hex characters of the digest, used as a
// shard so a project producing thousands of artefacts does not pile
// them into a single directory.
//
// The CAS stores bytes only — no metadata, no mode, no symlink
// tracking. The step cache's per-entry manifest (see step_cache.go)
// records (logical path → digest) so a cache hit can restore the
// declared outputs back into the working tree. Two outputs that
// happen to share content are deduplicated for free.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// CAS is a repo-local content-addressed store.
type CAS struct {
	// Root is the .sklp/cache/cas directory under the repo. Entries
	// live at Root/<aa>/<sha>. Created lazily by Put.
	Root string
}

// OpenCAS returns a CAS rooted at <repoAbs>/.sklp/cache/cas. The
// directory is created lazily by Put; OpenCAS itself never touches
// the filesystem.
func OpenCAS(repoAbs string) *CAS {
	return &CAS{Root: filepath.Join(repoAbs, ".sklp", "cache", "cas")}
}

// PutFile copies the file at absPath into the CAS and returns its
// content digest (hex SHA-256). The source file is left untouched.
// Existing entries with the same digest are preserved as-is — this
// is what gives the store its dedup property.
func (c *CAS) PutFile(absPath string) (string, error) {
	src, err := os.Open(absPath)
	if err != nil {
		return "", fmt.Errorf("cas: open source: %w", err)
	}
	defer src.Close()

	// First pass: hash. We can't stream-and-place in one go because
	// the destination path is itself a function of the content hash.
	h := sha256.New()
	if _, err := io.Copy(h, src); err != nil {
		return "", fmt.Errorf("cas: hash source: %w", err)
	}
	sum := hex.EncodeToString(h.Sum(nil))

	dst := c.pathFor(sum)
	if _, err := os.Stat(dst); err == nil {
		return sum, nil
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", fmt.Errorf("cas: mkdir shard: %w", err)
	}

	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("cas: rewind source: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".put-*")
	if err != nil {
		return "", fmt.Errorf("cas: temp: %w", err)
	}
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", fmt.Errorf("cas: copy: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("cas: close: %w", err)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("cas: rename: %w", err)
	}
	return sum, nil
}

// ErrCASMiss is returned by GetTo when the requested digest is absent
// from the store.
var ErrCASMiss = errors.New("cas: digest miss")

// GetTo restores the bytes of one stored digest to absDst. Parent
// directories of absDst are created as needed. The destination is
// overwritten if it exists. Returns ErrCASMiss if the digest is
// unknown.
func (c *CAS) GetTo(digest, absDst string) error {
	src, err := os.Open(c.pathFor(digest))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ErrCASMiss
		}
		return fmt.Errorf("cas: open digest: %w", err)
	}
	defer src.Close()

	if err := os.MkdirAll(filepath.Dir(absDst), 0o755); err != nil {
		return fmt.Errorf("cas: mkdir dst: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(absDst), ".get-*")
	if err != nil {
		return fmt.Errorf("cas: temp dst: %w", err)
	}
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("cas: copy out: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("cas: close dst: %w", err)
	}
	if err := os.Rename(tmp.Name(), absDst); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("cas: rename dst: %w", err)
	}
	return nil
}

// pathFor returns the on-disk path of one digest. The two-hex shard
// prefix keeps a single directory from exploding with thousands of
// entries.
func (c *CAS) pathFor(digest string) string {
	if len(digest) < 2 {
		return filepath.Join(c.Root, digest)
	}
	return filepath.Join(c.Root, digest[:2], digest)
}
