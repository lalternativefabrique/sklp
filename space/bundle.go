package space

import (
	"fmt"
	"os"
)

// nsPathsFor returns the kernel-stable /proc/<pid>/ns handles a joiner uses
// to setns into the leader's shared network + user namespaces.
func nsPathsFor(leaderInitPID int) *joinNamespaces {
	return &joinNamespaces{
		NetnsPath:  fmt.Sprintf("/proc/%d/ns/net", leaderInitPID),
		UsernsPath: fmt.Sprintf("/proc/%d/ns/user", leaderInitPID),
	}
}

// mkdirBundle creates a bundle directory. 0700: the bundle holds config.json
// with secrets in process.env, so it must not be world/group-traversable.
func mkdirBundle(bundle string) error {
	return os.MkdirAll(bundle, 0o700)
}
