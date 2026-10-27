//go:build !linux

package main

// listSpaceNames yields no candidates off-linux, where the runc-backed space
// registry does not build. Completion degrades to no dynamic names, never an error.
func listSpaceNames() ([]string, error) { return nil, nil }
