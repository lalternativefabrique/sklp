//go:build !linux

package space

// PurgeSpaceScratch is a no-op off Linux: the overlay writable-rootfs path only
// runs on Linux (a space is never launched elsewhere), so there is no scratch to
// reclaim. Defined so cross-platform callers (the deploy CLI) compile.
func PurgeSpaceScratch(string) error { return nil }
