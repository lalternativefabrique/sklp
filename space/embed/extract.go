package embed

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// binDir is the subdirectory under cacheRoot where extracted helper binaries
// live. Kept here (untagged) so callers and tests share one definition.
func binDir(cacheRoot string) string {
	return filepath.Join(cacheRoot, "runtime", "bin")
}

// extract writes content to <cacheRoot>/runtime/bin/<name> with mode 0755 and
// returns its path. Idempotent: if the file already exists with a matching
// sha256 it is left untouched. A stale file (size/content drift across an sklp
// upgrade) is rewritten atomically. Empty content is rejected so a missing
// embed (placeholder) surfaces loudly instead of producing a broken binary.
func extract(cacheRoot, name, wantSHA256 string, content []byte) (string, error) {
	if len(content) == 0 {
		return "", fmt.Errorf("embedded binary %q is empty: build was not run with the helper binaries present (run `go generate ./space/embed/`)", name)
	}
	got := sumHex(content)
	if subtle.ConstantTimeCompare([]byte(got), []byte(wantSHA256)) != 1 {
		return "", fmt.Errorf("embedded binary %q sha256 mismatch: got %s want %s (rerun `go generate ./space/embed/`)", name, got, wantSHA256)
	}
	dir := binDir(cacheRoot)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)

	want := sha256.Sum256(content)
	if existing, err := os.ReadFile(path); err == nil {
		if sha256.Sum256(existing) == want {
			// Ensure the bit survived a prior umask / copy.
			_ = os.Chmod(path, 0o755)
			return path, nil
		}
	}

	// Write atomically: a partial write must never leave an executable that
	// fails mid-launch.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, 0o755); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return path, nil
}

// sumHex is a small helper used by tests to assert extraction fidelity.
func sumHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
