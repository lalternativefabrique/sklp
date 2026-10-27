//go:build !linux

package sklet

// HostPreflight is a no-op off Linux (no rootless host prerequisites apply).
func HostPreflight() string { return "" }

// RunHostSetup is a stub off Linux: the rootless host prerequisites (uidmap,
// subuid, userns) are Linux-only concepts, so there is nothing to set up here.
func RunHostSetup() int { return 0 }
