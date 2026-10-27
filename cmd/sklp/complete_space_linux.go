//go:build linux

package main

import "github.com/lalternativefabrique/sklp/space"

// listSpaceNames resolves existing space names from the runc-backed runtime
// registry (socketless cache scan), the same way `space ls` discovers them.
func listSpaceNames() ([]string, error) {
	return space.ListSpaceNames()
}
