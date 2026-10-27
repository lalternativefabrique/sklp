//go:build linux

package space

import "github.com/lalternativefabrique/sklp/space/spacenet"

// subidChownTree takes ownership of a tree the rootless build left owned by
// mapped subuids, escalating into a userns when a plain chown hits EACCES/EPERM.
// A var so tests can stub the escalation (forking newuidmap is host-dependent).
var subidChownTree = func(path string) error { return spacenet.ChownTreeAsRoot(path) }
