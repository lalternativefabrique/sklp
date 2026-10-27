//go:build !linux

package main

import (
	"fmt"
	"os"
)

// runServe is unavailable off Linux: the node identity helpers it needs live in
// spacemanager/control's _linux files, and rootless spaces — what a CI node
// executes pipelines in — are Linux-only regardless.
func runServeInstall(o listenOptions) int {
	return runServe(o)
}

func runServe(o listenOptions) int {
	fmt.Fprintln(os.Stderr, "sklp run --listen: the node run API is Linux-only (it serves rootless spaces)")
	return 2
}
