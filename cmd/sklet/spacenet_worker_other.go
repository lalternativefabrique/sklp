//go:build !linux

package main

// maybeRunSpaceNetWorker is a no-op off Linux: the space network worker and the
// OCI hook re-execs are Linux-only concepts.
func maybeRunSpaceNetWorker() {}
