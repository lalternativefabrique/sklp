//go:build linux

package spacenet

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// cgroup_reap_linux.go reaps residual sklp deploy/runner cgroups left behind by
// a run that was interrupted (Ctrl-C, crash, SIGKILL) before its supervisor
// could tear the cgroup down. Two failure modes it fixes:
//
//   - An EMPTY cgroup dir (0 procs) that runc then refuses to reuse with
//     "container's cgroup is not empty" on the NEXT run (a stale dir with a
//     lingering controller ref). Safe to rmdir unconditionally.
//   - A cgroup holding ONLY a residual pause process ("sleep 2147483647") whose
//     supervisor is gone. Killing that one process empties the cgroup so the
//     next run recreates it cleanly.
//
// It is deliberately conservative: a caller supplies `protected`, the set of
// cgroup basenames that belong to a LIVE deployment (e.g. a running shop
// service). Those are never touched, procs or not.

// CgroupReapResult reports what a reap pass did (or would do, in dry-run).
type CgroupReapResult struct {
	EmptyRemoved []string // cgroup basenames that were (or would be) rmdir'd
	Killed       []int    // pids that were (or would be) killed to empty a cgroup
	Protected    []string // cgroup basenames skipped because they are live
}

// cgroupUserRoot is the systemd-delegated user slice sklp cgroups land under. A
// var so tests can point the reapers at a fixture tree instead of the real
// /sys/fs/cgroup, which no test may mutate.
var cgroupUserRoot = func() string {
	uid := strconv.Itoa(os.Getuid())
	return "/sys/fs/cgroup/user.slice/user-" + uid + ".slice/user@" + uid + ".service"
}

// CgroupUserRoot is the systemd-delegated user slice sklp cgroups land under.
func CgroupUserRoot() string { return cgroupUserRoot() }

// sklpCgroupRoots are the systemd-delegated slices where sklp deploy/runner
// cgroups land under `systemd --user`. The user@.service app.slice holds
// deploy services; the service root holds the on-the-fly runner cgroups.
func sklpCgroupGlobs() []string {
	base := cgroupUserRoot()
	return []string{
		filepath.Join(base, "app.slice", "sklp-deploy-*"),
		filepath.Join(base, "sklp-deploy-*"),
		filepath.Join(base, "sklp-*runner*"),
	}
}

// ReapUnownedCgroups reaps sklp cgroups whose basename is NOT claimed by any
// known space, killing whatever holds them. `owned` is the set of id prefixes of
// the spaces that still exist on disk: a cgroup matching one of them belongs to
// a space sklp can still reach through runc, so it is left to the normal
// teardown (and to `protected`, which additionally shields live containers).
//
// What remains is a cgroup whose space is gone — no runc state, no space dir, no
// `space down` that can touch it. Its processes have no supervisor and no other
// remedy than a manual kill, so unlike the pause-only rule of ReapOrphanCgroups
// this kills them whatever they are.
func ReapUnownedCgroups(owned []string, protected map[string]bool, dryRun bool, kill func(pid int) error, rmdir func(dir string) error) CgroupReapResult {
	var res CgroupReapResult
	seen := map[string]bool{}
	for _, glob := range sklpCgroupGlobs() {
		matches, _ := filepath.Glob(glob)
		for _, cg := range matches {
			if seen[cg] {
				continue
			}
			seen[cg] = true
			name := filepath.Base(cg)
			if protected[name] || hasAnyPrefix(name, owned) {
				continue
			}
			sub := reapCgroups([]string{cg}, protected, true, dryRun, kill, rmdir)
			res.EmptyRemoved = append(res.EmptyRemoved, sub.EmptyRemoved...)
			res.Killed = append(res.Killed, sub.Killed...)
			res.Protected = append(res.Protected, sub.Protected...)
		}
	}
	return res
}

func hasAnyPrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if p != "" && strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// CgroupProcsFor returns the pids still held by the sklp cgroup named exactly
// `name` (a container id). It is how a caller turns runc's opaque "container's
// cgroup is not empty" into the actual offending processes.
func CgroupProcsFor(name string) []int {
	if name == "" {
		return nil
	}
	base := cgroupUserRoot()
	for _, dir := range []string{
		filepath.Join(base, "app.slice", name),
		filepath.Join(base, name),
	} {
		if pids := cgroupProcs(dir); len(pids) > 0 {
			return pids
		}
	}
	return nil
}

// isResidualPause reports whether pid is a lone container pause process (runc's
// init keep-alive). Two forms are recognized: the current `sklp pause` (arg0
// basename "sklp" — baked at /usr/local/bin/sklp or bind-mounted at
// /.sklp/pause — with a "pause" arg), and the legacy `sleep <big>` left by
// pre-pause runs during the upgrade window. Only such a proc is safe to kill to
// empty a stranded cgroup; anything else means real work is running and we must
// NOT touch it (return false → the cgroup is left alone).
// procCmdline reads a pid's argv. A var so tests can exercise the recognition
// rules without spawning real processes.
var procCmdline = func(pid int) ([]byte, error) {
	return os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
}

func isResidualPause(pid int) bool {
	raw, err := procCmdline(pid)
	if err != nil {
		return false // gone already, or unreadable — don't claim it as ours
	}
	// cmdline is NUL-separated.
	args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	if len(args) < 1 {
		return false
	}
	// Current form: `sklp pause`. arg0 is the baked binary ("sklp") when the
	// recipe installed it, but the BIND-MOUNTED form is /.sklp/pause, whose
	// basename is "pause" — accept both or every bind-mounted runner's pause
	// reads as real work and its cgroup is never reclaimed.
	if base := filepath.Base(args[0]); base == "sklp" || base == "pause" {
		for _, a := range args[1:] {
			if a == "pause" {
				return true
			}
		}
		return false
	}
	// Legacy form: `sleep <big>`. The keep-alive uses a huge sleep; treat any
	// multi-year sleep as the pause. Drop this branch a few releases out.
	if len(args) == 2 && filepath.Base(args[0]) == "sleep" {
		n, err := strconv.Atoi(args[1])
		return err == nil && n > 31_536_000 // > 1 year in seconds
	}
	return false
}

// cgroupProcs reads the pids in a cgroup.procs file. A missing/empty file
// yields nil (an empty cgroup), which the caller treats as "safe to rmdir".
func cgroupProcs(cgDir string) []int {
	raw, err := os.ReadFile(filepath.Join(cgDir, "cgroup.procs"))
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Fields(string(raw)) {
		if p, err := strconv.Atoi(line); err == nil {
			pids = append(pids, p)
		}
	}
	return pids
}

// ReapOrphanCgroups removes empty sklp cgroups and empties cgroups holding only
// a residual pause process, skipping any basename in `protected`. When dryRun is
// true it reports what it WOULD do and mutates nothing. kill/rmdir are injected
// so tests drive the pure selection logic without root.
func ReapOrphanCgroups(protected map[string]bool, dryRun bool, kill func(pid int) error, rmdir func(dir string) error) CgroupReapResult {
	return reapCgroups(sklpCgroupGlobs(), protected, false, dryRun, kill, rmdir)
}

// ReapCgroupsByPrefix reaps every sklp cgroup whose basename starts with prefix,
// killing whatever holds it — a real service process, not just a pause. It is
// the recovery path for a space whose state dir is gone: with no runc state left
// there is no container to `runc kill`, so the cgroup is the only remaining
// handle on the leaked processes, and the next run cannot reuse the id until
// they die. Callers must pass a SPACE-SCOPED prefix (containerIDPrefix), never
// the bare "sklp-deploy-", or a teardown of one space kills another's services.
func ReapCgroupsByPrefix(prefix string, protected map[string]bool, dryRun bool, kill func(pid int) error, rmdir func(dir string) error) CgroupReapResult {
	if prefix == "" {
		return CgroupReapResult{}
	}
	base := cgroupUserRoot()
	globs := []string{
		filepath.Join(base, "app.slice", prefix+"*"),
		filepath.Join(base, prefix+"*"),
	}
	return reapCgroups(globs, protected, true, dryRun, kill, rmdir)
}

// reapCgroups is the shared selection pass. killAny widens the non-empty case
// from "only if every proc is a residual pause" to "kill whatever is in there",
// which is safe ONLY when the caller has already established the cgroup belongs
// to a space it is tearing down.
func reapCgroups(globs []string, protected map[string]bool, killAny, dryRun bool, kill func(pid int) error, rmdir func(dir string) error) CgroupReapResult {
	var res CgroupReapResult
	seen := map[string]bool{}
	for _, glob := range globs {
		matches, _ := filepath.Glob(glob)
		for _, cg := range matches {
			if seen[cg] {
				continue
			}
			seen[cg] = true
			name := filepath.Base(cg)
			if protected[name] {
				res.Protected = append(res.Protected, name)
				continue
			}
			procs := cgroupProcs(cg)
			if len(procs) == 0 {
				res.EmptyRemoved = append(res.EmptyRemoved, name)
				if !dryRun {
					_ = rmdir(cg)
				}
				continue
			}
			// Non-empty: only reap if EVERY proc is a residual pause. If any
			// proc is real work, leave the whole cgroup untouched.
			if !killAny {
				allPause := true
				for _, p := range procs {
					if !isResidualPause(p) {
						allPause = false
						break
					}
				}
				if !allPause {
					res.Protected = append(res.Protected, name)
					continue
				}
			}
			for _, p := range procs {
				res.Killed = append(res.Killed, p)
				if !dryRun {
					_ = kill(p)
				}
			}
			res.EmptyRemoved = append(res.EmptyRemoved, name)
			if !dryRun {
				_ = rmdir(cg)
			}
		}
	}
	return res
}
