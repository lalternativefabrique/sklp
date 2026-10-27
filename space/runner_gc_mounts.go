package space

import (
	"os"
	"strings"
)

// mountedRunnerPaths returns the runner cache paths the kernel currently has
// mounted, as substrings found in the mount table. A runner rootfs backs a
// service two ways — bind-mounted verbatim, or as the lowerdir of the per-service
// overlay (overlay_linux.go) — and a persistent space keeps either alive long
// after the process that built it exited, so the pid lock cannot answer for it.
// The mount table can: it is the kernel's own record of what would break.
//
// Matching is by substring over the whole line because a lowerdir appears in
// the super-options field (lowerdir=/…/runner/<hash>/rootfs), not the mount
// point, and an overlay stacks several colon-separated lowers in one option.
func mountedRunnerPaths() []string {
	blob, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	return strings.Split(string(blob), "\n")
}

// runnerEntryMounted reports whether any current mount references entryPath.
// The trailing separator matters: hashes are hex of a fixed length today, but
// nothing enforces that one is never a prefix of another, and a bare
// strings.Contains would then report the wrong entry live — an error that
// silently keeps a dead multi-GiB tree forever.
func runnerEntryMounted(lines []string, entryPath string) bool {
	if entryPath == "" {
		return false
	}
	for _, line := range lines {
		if strings.Contains(line, entryPath+"/") || strings.HasSuffix(line, entryPath) {
			return true
		}
	}
	return false
}
