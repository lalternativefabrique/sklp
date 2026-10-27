//go:build !linux

package space

// subidChownTree is a no-op off Linux: there is no userns/subid mapping, so a
// build writes the rootfs as the invoking user already.
var subidChownTree = func(string) error { return nil }
