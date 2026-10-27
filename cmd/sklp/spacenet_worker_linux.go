//go:build linux

package main

import (
	"github.com/lalternativefabrique/sklp/space/ociproc"
	"github.com/lalternativefabrique/sklp/space/spacenet"
)

// maybeRunSpaceNetWorker turns this process into the space network worker (or
// netns holder) when re-exec'd by spacenet, then exits — it never returns in
// that case. On a normal invocation it is a no-op. It MUST run before any
// other work in main() so the re-exec takes effect, mirroring the Landlock
// sandbox shim. Only the space-deploy build (embedded_runtime+linux) wires
// it; all other builds get the no-op below.
//
// It also handles the paired OCI hook re-execs: createRuntime moves the veth
// through the space worker, createContainer configures it from inside the
// container netns, and the build-path blackhole hook installs the egress
// filter inside a build step's netns.
func maybeRunSpaceNetWorker() {
	spacenet.MaybeRunWorker()
	spacenet.MaybeRunSubidRemove()
	spacenet.MaybeRunAttachHook()
	ociproc.MaybeRunBlackholeHook()
}
