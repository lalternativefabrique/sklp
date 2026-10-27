//go:build linux

package spacenet

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// slirpMarker is an argv token unique to the slirp4netns processes spacenet (and
// the CI slirp path) start. Combined with the trailing "tap0" device name it
// identifies one of OUR egress slirps; we never touch a slirp without it.
const slirpMarker = "--disable-host-loopback"

// procEntry is one scanned process: its pid and full argv. Abstracted so the
// reaper's pure logic is unit-testable without a real /proc.
type procEntry struct {
	pid  int
	argv []string
}

// reapOrphanSlirp kills every orphaned egress slirp4netns: one whose target
// netns pid (the argv token before "tap0") is no longer a live process. slirp
// runs on the HOST stack, so a space that died without Close (crash, kill -9)
// leaves its slirp reparented to PID 1, leaking an FD/netns/memory. Run at
// startup, best-effort. It NEVER kills a slirp whose target pid is still alive.
func reapOrphanSlirp() {
	procs, err := listProcs()
	if err != nil {
		return // best-effort: no /proc access => nothing to reap
	}
	for _, victim := range orphanSlirpPIDs(procs, pidAlive) {
		_ = syscall.Kill(victim, syscall.SIGKILL)
	}
}

// orphanSlirpPIDs is the pure selector: from all processes, return the pids of
// slirp4netns instances we own whose target netns pid is dead (per alive). A
// slirp is ours iff its argv carries slirpMarker and ends in the "tap0" device;
// the token immediately before "tap0" is the target pid. Exported logic kept
// side-effect-free so a test drives it with synthetic procs and an alive stub.
func orphanSlirpPIDs(procs []procEntry, alive func(pid int) bool) []int {
	var victims []int
	for _, p := range procs {
		if len(p.argv) < 2 {
			continue
		}
		if !argvIsOurSlirp(p.argv) {
			continue
		}
		target, ok := slirpTargetPID(p.argv)
		if !ok {
			continue
		}
		if !alive(target) {
			victims = append(victims, p.pid)
		}
	}
	return victims
}

// argvIsOurSlirp reports whether argv is one of spacenet's slirp4netns processes:
// its program base name contains "slirp4netns", it carries the marker flag, and
// its last token is the "tap0" device. All three together avoid matching an
// unrelated slirp the user may run.
func argvIsOurSlirp(argv []string) bool {
	if !strings.Contains(filepath.Base(argv[0]), "slirp4netns") {
		return false
	}
	if argv[len(argv)-1] != slirpTap {
		return false
	}
	for _, a := range argv {
		if a == slirpMarker {
			return true
		}
	}
	return false
}

// slirpTargetPID extracts the target netns pid: slirp4netns is invoked as
// "... <pid> tap0", so the token before the trailing device is the pid.
func slirpTargetPID(argv []string) (int, bool) {
	if len(argv) < 2 {
		return 0, false
	}
	pid, err := strconv.Atoi(argv[len(argv)-2])
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// pidAlive reports whether pid names a live, non-zombie process. A dead target
// netns owner is the orphan signal. Signal 0 probes existence; we also reject a
// zombie since its netns is already gone.
func pidAlive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false // ESRCH (gone) or EPERM (not ours) => treat as not-alive-ours
	}
	return procState(pid) != 'Z'
}

// procState returns the single-char process state from /proc/<pid>/stat ('Z' for
// zombie), or 0 when it cannot be read. Used to exclude zombies from "alive".
func procState(pid int) byte {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	// Format: "pid (comm) STATE ...". State is the token after the last ')'.
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 || i+2 >= len(b) {
		return 0
	}
	return b[i+2]
}

// listProcs scans /proc for every process and its argv (NUL-delimited cmdline).
func listProcs() ([]procEntry, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var out []procEntry
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // non-pid entry
		}
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil || len(b) == 0 {
			continue
		}
		argv := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
		out = append(out, procEntry{pid: pid, argv: argv})
	}
	return out, nil
}
