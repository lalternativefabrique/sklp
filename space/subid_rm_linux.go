//go:build linux

package space

import "github.com/lalternativefabrique/sklp/space/spacenet"

// subidRemoveAll removes path, escalating into a userns to delete a subuid-owned
// tree (a container that ran as non-root) when a plain remove hits EACCES/EPERM.
// A var so tests can stub the escalation (forking newuidmap is host-dependent).
var subidRemoveAll = func(path string) error { return spacenet.RemoveAllAsRoot(path) }
