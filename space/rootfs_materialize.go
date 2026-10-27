package space

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// materializeRootfs builds rootfs/<digest> exactly once, whatever the number
// of services asking for it at the same moment. Callers hold an exclusive
// flock on the digest for the whole build, so a second one waits and then
// finds the sentinel; the tree is filled in a staging directory and renamed
// into place, so a reader never sees a half-extracted rootfs carrying the
// sentinel. fill receives the staging directory.
func materializeRootfs(cacheRoot, digest string, fill func(staging string) error) (string, error) {
	rootfsDir := filepath.Join(cacheRoot, "rootfs")
	if err := os.MkdirAll(rootfsDir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(rootfsDir, digest)
	if _, err := os.Stat(filepath.Join(dst, ".extracted")); err == nil {
		return dst, nil
	}
	unlock, err := lockDigest(rootfsDir, digest)
	if err != nil {
		return "", err
	}
	defer unlock()
	if _, err := os.Stat(filepath.Join(dst, ".extracted")); err == nil {
		return dst, nil
	}
	staging := dst + ".staging"
	for _, stale := range []string{staging, dst} {
		if err := subidRemoveAll(stale); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return "", err
	}
	if err := fill(staging); err != nil {
		_ = subidRemoveAll(staging)
		return "", err
	}
	if err := os.WriteFile(filepath.Join(staging, ".extracted"), nil, 0o644); err != nil {
		_ = os.RemoveAll(staging)
		return "", err
	}
	if err := os.Rename(staging, dst); err != nil {
		_ = os.RemoveAll(staging)
		return "", err
	}
	return dst, nil
}

func lockDigest(rootfsDir, digest string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(rootfsDir, digest+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open rootfs lock %s: %w", digest, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock rootfs %s: %w", digest, err)
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}, nil
}
