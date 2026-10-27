//go:build linux

package runnerbuild

import (
	"os"
	"strings"
)

// NestedUserns reports whether this process already runs inside a user
// namespace, where `unshare --map-users=1:100000:65535` is refused: a child
// uid_map may only name uids the parent namespace itself maps, and a rootless
// parent maps uid 0 alone. The build then runs with uid 0 as its only mapping,
// apt cannot seteuid to _apt (42), and dpkg fails mid-install.
//
// /proc/self/uid_map holds the identity map "0 0 4294967295" on the host — a
// single line mapping the whole uid space. Inside a namespace the range is
// narrow (e.g. "0 100000 1" on a rootless host, or "0 0 1" when the parent
// itself runs as root, as in a privileged CI pod): the third field, the range
// length, is what distinguishes nesting, not the offset — a privileged pod's
// nested map can legitimately start at 0. Checking the offset alone (as this
// used to) misreads "0 0 1" as the host identity map and skips the nested
// path, so `unshare --user` inside the build fails outright. Unreadable /proc
// means no evidence of nesting, so the host mapping stands.
func NestedUserns() bool {
	b, err := os.ReadFile("/proc/self/uid_map")
	if err != nil {
		return false
	}
	f := strings.Fields(string(b))
	return len(f) < 3 || f[2] != "4294967295"
}
