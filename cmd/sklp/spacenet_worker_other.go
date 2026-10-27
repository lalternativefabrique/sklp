//go:build !linux

package main

// maybeRunSpaceNetWorker is a no-op off the space-deploy build: spacenet (the
// rootless space bridge) only exists under embedded_runtime+linux.
func maybeRunSpaceNetWorker() {}
