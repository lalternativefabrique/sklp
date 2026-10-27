//go:build !linux

package spawn

import (
	"context"
	"fmt"
	"runtime"
)

// spawnEmbedded is the non-Linux half of the OCI spawner. The space runtime it
// drives on Linux (namespaces, cgroups, overlayfs, runc) has no equivalent
// outside that kernel, so a pipeline cannot execute in-process here: the darwin
// binary relays the whole invocation to a Linux VM before dispatch ever reaches
// a spawner.
func (s *OCISpawner) spawnEmbedded(ctx context.Context, cfg Config) (string, error) {
	return "", fmt.Errorf("oci spawner: no local container runtime on %s", runtime.GOOS)
}
