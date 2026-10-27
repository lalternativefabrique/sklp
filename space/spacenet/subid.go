//go:build linux

package spacenet

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"errors"
	"github.com/lalternativefabrique/sklp/space/ociproc"
	"golang.org/x/sys/unix"
)

// resolveSubidRange returns the supplementary subuid/subgid range this user is
// actually granted in /etc/subuid + /etc/subgid (container uid/gid 1..count ->
// host base..). It reads the SAME source ociproc.ResolveIDMappings uses for the
// service containers, so the worker maps the identical range whichever path
// (runc or this worker) creates the userns. Reading it dynamically — rather than
// hardcoding 100000 — is what lets a user whose base is not 100000 (e.g. the 2nd
// cloud-init user lands at 165536) come up instead of failing newuidmap with
// "uid range ... not allowed".
func resolveSubidRange(hostUID int) (uidBase, gidBase, count int, err error) {
	return ociproc.ResolveSubIDBase(hostUID)
}

// subidTools resolves the setuid helpers that write a multi-range uid_map /
// gid_map for an unprivileged process. The Go runtime can only write a
// single-range map covering the caller's own id directly to /proc/<pid>/id_map;
// a subuid RANGE requires newuidmap/newgidmap, which are setuid-root and
// validate the request against /etc/subuid + /etc/subgid. Both must resolve for
// the range path to be taken; otherwise the caller falls back to size-1.
type subidTools struct {
	newuidmap string
	newgidmap string
}

// lookupSubidTools finds newuidmap/newgidmap on PATH and a subuid range for
// the current user. ok is false (and the caller must fall back to the
// single-id mapping) when either helper or the range is missing: a host with
// uidmap installed but no /etc/subuid entry — root inside a container built
// from the host packages — has no range to map any more than one without the
// tools, and must degrade the same way instead of refusing the space.
func lookupSubidTools() (subidTools, bool) {
	uid, err := exec.LookPath("newuidmap")
	if err != nil {
		return subidTools{}, false
	}
	gid, err := exec.LookPath("newgidmap")
	if err != nil {
		return subidTools{}, false
	}
	if _, _, _, err := resolveSubidRange(os.Getuid()); err != nil {
		return subidTools{}, false
	}
	return subidTools{newuidmap: uid, newgidmap: gid}, true
}

// newuidmapArgs builds the newuidmap argv that maps, for worker pid, container
// id 0 -> hostUID (size 1) and container ids 1..count -> base.. .
// newuidmap takes triplets: "<pid> <containerID> <hostID> <size> ...".
func newuidmapArgs(pid, hostUID, base, count int) []string {
	return []string{
		fmt.Sprintf("%d", pid),
		"0", fmt.Sprintf("%d", hostUID), "1",
		"1", fmt.Sprintf("%d", base), fmt.Sprintf("%d", count),
	}
}

// newgidmapArgs is newuidmapArgs for the gid map (container gid 0 -> hostGID).
func newgidmapArgs(pid, hostGID, base, count int) []string {
	return []string{
		fmt.Sprintf("%d", pid),
		"0", fmt.Sprintf("%d", hostGID), "1",
		"1", fmt.Sprintf("%d", base), fmt.Sprintf("%d", count),
	}
}

// runStage1 implements the worker's first stage on the subuid-range path. The
// process was cloned into an EMPTY userns, so it currently holds NO effective
// capabilities (CapEff=0): the kernel grants userns caps only at the next
// execve boundary, after a uid_map is in place. So stage 1 (1) blocks until the
// parent writes the maps via newuidmap/newgidmap (EOF on the sync pipe), then
// (2) re-execs the same binary as stage 2 — which starts as uid 0 in the now
// fully-mapped userns and thus holds full caps to build the bridge. It never
// returns: it either execve's stage 2 or exits non-zero. fds 0/1/2 and the
// ingress fd survive execve (none is close-on-exec in this child).
func runStage1() {
	waitSubidMapping()

	// Re-exec self as stage 2. Strip the sync fd env (consumed) and flip the
	// stage marker; everything else (bridge/gateway/ingress env, argv, fds) is
	// inherited unchanged so stage 2 sees the same configuration.
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "spacenet: stage1 resolve self: %v\n", err)
		os.Exit(1)
	}
	env := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, envSyncFD+"=") || strings.HasPrefix(kv, workerEnv+"=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, workerEnv+"=2")
	if err := syscall.Exec(self, os.Args, env); err != nil {
		fmt.Fprintf(os.Stderr, "spacenet: stage1 re-exec: %v\n", err)
		os.Exit(1)
	}
}

// waitSubidMapping blocks until the parent has finished writing this process's
// uid_map/gid_map, signalled by EOF on the sync pipe (envSyncFD). The read
// drains any bytes too, so the parent may also signal by writing. A missing or
// malformed fd returns immediately rather than hanging forever.
func waitSubidMapping() {
	v := os.Getenv(envSyncFD)
	if v == "" {
		return
	}
	fd, err := strconv.Atoi(v)
	if err != nil {
		return
	}
	f := os.NewFile(uintptr(fd), "space-subid-sync")
	if f == nil {
		return
	}
	defer f.Close()
	_, _ = io.Copy(io.Discard, f) // returns on EOF (parent closed its end)
}

// resolveIdentity is the worker's identity from the caller's subuid/subgid
// grant, as resolveSubidRange reads it: one count for both ranges.
//
// Root of the initial namespace (the macOS vz guest runs sklp as root) gets an
// empty identity from ociproc; the worker still needs its /etc/subuid range,
// or newuidmap is handed a zero-sized range and refuses it as an overflow.
func resolveIdentity(hostUID, hostGID int) (ociproc.Identity, error) {
	id, err := ociproc.ResolveIdentity(hostUID, hostGID)
	if err != nil {
		return ociproc.Identity{}, err
	}
	if id.UIDCount == 0 || id.GIDCount == 0 {
		uidBase, gidBase, count, err := resolveSubidRange(hostUID)
		if err != nil {
			return ociproc.Identity{}, err
		}
		id = ociproc.Identity{HostUID: hostUID, HostGID: hostGID, UIDBase: uidBase, GIDBase: gidBase, UIDCount: count, GIDCount: count}
	}
	count := min(id.UIDCount, id.GIDCount)
	id.UIDCount, id.GIDCount = count, count
	return id, nil
}

// applySubidRange writes the worker's full uid/gid maps via the setuid helpers.
// Order matters: gid_map of a process that has not had setgroups disabled is
// rejected by the kernel, but newgidmap performs the setgroups=deny write
// itself, so calling it (rather than writing /proc directly) is sufficient.
func (t subidTools) applySubidRange(pid int, id ociproc.Identity) error {
	uidArgs := newuidmapArgs(pid, id.HostUID, id.UIDBase, id.UIDCount)
	gidArgs := newgidmapArgs(pid, id.HostGID, id.GIDBase, id.GIDCount)
	if id.RootPoolCount > 0 {
		uidArgs = append(uidArgs, fmt.Sprintf("%d", id.UIDCount+1), fmt.Sprintf("%d", id.RootPoolUID), fmt.Sprintf("%d", id.RootPoolCount))
		gidArgs = append(gidArgs, fmt.Sprintf("%d", id.GIDCount+1), fmt.Sprintf("%d", id.RootPoolGID), fmt.Sprintf("%d", id.RootPoolCount))
	}
	if out, err := runMapHelper(t.newuidmap, uidArgs); err != nil {
		return fmt.Errorf("newuidmap pid %d: %w: %s", pid, err, out)
	}
	if out, err := runMapHelper(t.newgidmap, gidArgs); err != nil {
		return fmt.Errorf("newgidmap pid %d: %w: %s", pid, err, out)
	}
	return nil
}

// runMapHelper runs a setuid id-mapping helper, tolerating the exit status
// being harvested by someone else.
//
// The supervisor that calls this also installs a SIGCHLD subreaper whose drain
// loop does Wait4(-1): it reaps EVERY child, this helper included, and os/exec
// then finds nothing left to wait on — "waitid: no child processes". The helper
// RAN, and its work (the uid_map write) is already done; only the status was
// taken. Reporting that as a failure took a space down at boot, on a node whose
// mapping was in fact correct.
//
// A genuine failure still surfaces: the helper writes to stderr and exits
// non-zero, which is a different error than ECHILD.
func runMapHelper(bin string, args []string) ([]byte, error) {
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil && errors.Is(err, unix.ECHILD) {
		return out, nil
	}
	return out, err
}
