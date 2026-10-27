//go:build !linux

package space

// reapSpaceCgroups is a no-op off Linux (no cgroups v2 tree to drain).
var reapSpaceCgroups = func(prefix string) {}
