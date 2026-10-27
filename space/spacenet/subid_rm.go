//go:build linux

package spacenet

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// rmEnv marks the re-exec'd subuid-remove child (stage 1 -> stage 2, like the
// space worker); envRmPath carries the tree it removes. Mirrors the handshake.
const (
	rmEnv     = "SKLP_SUBID_RM"
	envRmPath = "SKLP_SUBID_RM_PATH"
	// envRmOp selects what the child does with envRmPath. Absent means remove,
	// so a stage-2 child from an older binary keeps its meaning.
	envRmOp = "SKLP_SUBID_RM_OP"

	opRemove = "remove"
	opChown  = "chown"
	// opRetarget changes the owner of every file owned by envRmFrom to
	// envRmTo, both container ids of the mapped userns; nothing else moves.
	opRetarget = "retarget"
	envRmFrom  = "SKLP_SUBID_RM_FROM"
	envRmTo    = "SKLP_SUBID_RM_TO"
	// opOwners gives each path of the list in envRmList the container ids
	// recorded beside it, under envRmPath.
	opOwners  = "owners"
	envRmList = "SKLP_SUBID_RM_LIST"
)

// RemoveAllAsRoot removes path, escalating into a userns only when needed. It
// first tries a plain os.RemoveAll (operator-owned tree); on EACCES/EPERM it
// re-execs self into a fresh userns with the full subuid range mapped, so a tree
// owned by a mapped subuid (a container that ran as non-root) is seen as uid 0
// and removable. When the subuid helpers are absent it returns the original
// permission error wrapped, matching the best-effort CleanCache fallback.
func RemoveAllAsRoot(path string) error {
	if err := os.RemoveAll(path); err == nil {
		return nil
	} else if !permErr(err) {
		return err
	}
	// EACCES from a tree WE own: the Go module cache marks every file+dir
	// read-only (0444/0555), so os.RemoveAll cannot unlink inside them. This is
	// what `go clean -modcache` works around. chmod +w the whole tree and retry
	// before assuming a subuid-owned tree — the far more common local case.
	if chmodTreeWritable(path) == nil {
		if err := os.RemoveAll(path); err == nil {
			return nil
		}
	}
	tools, ok := lookupSubidTools()
	if !ok {
		return fmt.Errorf("remove %q: subuid range unavailable (newuidmap/newgidmap missing): %w", path, os.ErrPermission)
	}
	return removeInUserns(tools, path)
}

// chmodTreeWritable adds owner-write to every dir and file under root so a
// subsequent os.RemoveAll can unlink read-only Go module cache entries. Dirs
// need +w to remove their children; best-effort per entry so one odd file does
// not abort the whole walk.
func chmodTreeWritable(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries; RemoveAll will report if fatal
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		_ = os.Chmod(p, info.Mode().Perm()|0o200)
		return nil
	})
}

// ChownTreeAsRoot rewrites ownership of everything under path to the calling
// user, escalating into a userns when the tree is owned by mapped subuids.
//
// A rootless build writes its rootfs through a uid map, so any image that chowns
// to a non-root user (`chown postgres:postgres`, or just an apk package that
// ships one) leaves directories owned by 100000+. The operator can neither read
// nor chmod those — ownership, not mode, is what denies access — so a plain copy
// of that rootfs fails. Taking ownership first is what makes it readable.
//
// It is a no-op when the tree is already owned: the walk succeeds and every
// chown is a cheap self-assignment. When the subuid helpers are missing it
// returns the permission error rather than silently copying a partial tree.
func ChownTreeAsRoot(path string) error {
	uid, gid := os.Getuid(), os.Getgid()
	if err := chownTree(path, uid, gid); err == nil {
		return nil
	} else if !permErr(err) {
		return err
	}
	tools, ok := lookupSubidTools()
	if !ok {
		return fmt.Errorf("chown %q: subuid range unavailable (newuidmap/newgidmap missing): %w", path, os.ErrPermission)
	}
	return runInUserns(tools, opChown, path)
}

// chownTree walks root and chowns every entry to uid:gid. Inside the userns
// child the caller is uid 0 and the operator's real uid is what it maps FROM,
// so the child passes its own (already mapped) ids — which is why this takes
// them as arguments instead of reading them itself.
//
// Symlinks are chowned with Lchown so the link, not its target, changes hands;
// following one could rewrite ownership outside the tree entirely.
// RetargetTreeOwner hands every file under path owned by fromID to toID,
// both container ids of the space mapping (0 is the operator, a space root
// is its slot id). Files owned by any other id, a service's own uid for
// instance, keep their owner: their mapping is the same before and after.
// This is the migration of a volume between the shared and the child
// namespace, run once and explicitly; a start never does it.
func RetargetTreeOwner(path string, fromID, toID int) error {
	tools, ok := lookupSubidTools()
	if !ok {
		return fmt.Errorf("retarget %q: subuid range unavailable (newuidmap/newgidmap missing): %w", path, os.ErrPermission)
	}
	return runInUserns(tools, opRetarget, path, fmt.Sprintf("%s=%d", envRmFrom, fromID), fmt.Sprintf("%s=%d", envRmTo, toID))
}

// ApplyOwnerList gives the files under root the container ids an image layer
// declared for them. listFile holds one "uid gid relpath" line per file; ids
// are container ids, so the tree ends up owned by the subuids a container of
// this image sees as those ids. Without subuid delegation it returns
// os.ErrPermission and the tree stays the operator's.
func ApplyOwnerList(root, listFile string) error {
	tools, ok := lookupSubidTools()
	if !ok {
		return fmt.Errorf("apply owners under %q: subuid range unavailable (newuidmap/newgidmap missing): %w", root, os.ErrPermission)
	}
	return runInUserns(tools, opOwners, root, envRmList+"="+listFile)
}

// applyOwnerList resolves every path through os.Root: the list comes from an
// image's tar headers, and a later layer can turn a parent directory into a
// symlink pointing out of the rootfs.
func applyOwnerList(root, listFile string) error {
	data, err := os.ReadFile(listFile)
	if err != nil {
		return err
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, line := range strings.Split(string(data), "\n") {
		uidStr, rest, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		gidStr, rel, ok := strings.Cut(rest, " ")
		if !ok {
			continue
		}
		uid, err1 := strconv.Atoi(uidStr)
		gid, err2 := strconv.Atoi(gidStr)
		if err1 != nil || err2 != nil {
			return fmt.Errorf("owner list: bad line %q", line)
		}
		name := strings.TrimPrefix(filepath.Clean("/"+rel), "/")
		if name == "" {
			name = "."
		}
		if err := r.Lchown(name, uid, gid); err != nil && !errors.Is(err, os.ErrNotExist) && !isRootEscape(err) {
			return err
		}
	}
	return nil
}

func retargetTree(root string, from, to int) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(st.Uid) != from {
			return nil
		}
		gid := int(st.Gid)
		if gid == from {
			gid = to
		}
		return os.Lchown(p, to, gid)
	})
}

func chownTree(root string, uid, gid int) error {
	return walkChown(root, uid, gid)
}

// walkChown descends manually rather than through filepath.WalkDir: a directory
// the image left without u+x cannot be listed until it is chowned, and WalkDir
// would have to list it first. Taking ownership on the way down is what opens
// the next level.
func walkChown(dir string, uid, gid int) error {
	if err := os.Lchown(dir, uid, gid); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Owning a directory is not the same as being able to read it: a 0600 dir
	// still refuses the listing below. Chmod answers to ownership, which the
	// Lchown above has just secured, so widen before descending.
	if info, err := os.Lstat(dir); err == nil && info.Mode().Perm()&0o700 != 0o700 {
		_ = os.Chmod(dir, info.Mode().Perm()|0o700)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if e.IsDir() && e.Type()&fs.ModeSymlink == 0 {
			if err := walkChown(p, uid, gid); err != nil {
				return err
			}
			continue
		}
		if err := os.Lchown(p, uid, gid); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// permErr reports whether err is a permission error (EACCES/EPERM) — the signal
// that path is owned by a mapped subuid the operator cannot delete directly.
func permErr(err error) bool {
	return errors.Is(err, os.ErrPermission) ||
		errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM)
}

// removeInUserns forks self into an EMPTY userns parked on a sync pipe, writes
// the full subuid range from the parent (so 100000+ files map to uid 0 in the
// child), then releases the child to RemoveAll the path. Same handshake as the
// space worker (see spacenet.go / runStage1).
func removeInUserns(tools subidTools, path string) error {
	return runInUserns(tools, opRemove, path)
}

// runInUserns forks self into an EMPTY userns parked on a sync pipe, writes the
// full subuid range from the parent (so 100000+ files map to uid 0 in the
// child), then releases the child to apply op to path.
func runInUserns(tools subidTools, op, path string, env ...string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("subid rm: resolve self: %w", err)
	}
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), rmEnv+"=1", envRmPath+"="+path, envRmOp+"="+op)
	cmd.Env = append(cmd.Env, env...)
	cmd.Stderr = os.Stderr
	id, err := resolveIdentity(os.Getuid(), os.Getgid())
	if err != nil {
		return fmt.Errorf("subid rm: map subuid range: %w", err)
	}
	err = startMapped(cmd, func(pid int) error { return tools.applySubidRange(pid, id) })
	if err != nil {
		return fmt.Errorf("subid rm: start: %w", err)
	}
	// The supervisor's SIGCHLD subreaper may have harvested the exit status
	// first (see runMapHelper): the helper ran, only its status is gone.
	if err := cmd.Wait(); err != nil && !errors.Is(err, unix.ECHILD) {
		return fmt.Errorf("subid %s %q: %w", op, path, err)
	}
	return nil
}

// MaybeRunSubidRemove turns this process into the subuid-remove child when the
// marker is set, then exits — it never returns in that case. On a normal
// invocation it is a no-op. Hosts that call RemoveAllAsRoot MUST call it early
// in main(), alongside MaybeRunWorker.
//
// Two stages mirror the space worker: stage 1 was cloned into an EMPTY userns so
// it holds NO caps (CapEff=0); it waits for the parent to write the maps then
// re-execs into stage 2, which starts as uid 0 in the now-mapped userns and thus
// holds CAP_DAC_OVERRIDE — required to unlink a subuid-owned tree.
func MaybeRunSubidRemove() {
	switch os.Getenv(rmEnv) {
	case "1":
		runRmStage1() // never returns: execve's stage 2 or exits on failure
	case "2":
		path := os.Getenv(envRmPath)
		op := os.Getenv(envRmOp)
		if op == "" {
			op = opRemove
		}
		var err error
		switch op {
		case opChown:
			// uid 0 here IS the operator's host uid: the map sends container 0
			// to hostUID (see newuidmapArgs), so chowning to 0:0 in this ns
			// hands the tree to the operator outside it.
			err = chownTree(path, os.Getuid(), os.Getgid())
		case opOwners:
			err = applyOwnerList(path, os.Getenv(envRmList))
		case opRetarget:
			from, ferr := strconv.Atoi(os.Getenv(envRmFrom))
			to, terr := strconv.Atoi(os.Getenv(envRmTo))
			if ferr != nil || terr != nil {
				err = fmt.Errorf("retarget: bad ids %q -> %q", os.Getenv(envRmFrom), os.Getenv(envRmTo))
			} else {
				err = retargetTree(path, from, to)
			}
		default:
			err = os.RemoveAll(path)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "subid %s child: %q: %v\n", op, path, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
}

// runRmStage1 blocks until the parent has written the uid/gid maps (EOF on the
// sync pipe), then re-execs self as stage 2 so the new image picks up the userns
// caps at the execve boundary. It never returns.
func runRmStage1() {
	waitSubidMapping()
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "subid rm: stage1 resolve self: %v\n", err)
		os.Exit(1)
	}
	env := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, envSyncFD+"=") || strings.HasPrefix(kv, rmEnv+"=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, rmEnv+"=2")
	if err := syscall.Exec(self, os.Args, env); err != nil {
		fmt.Fprintf(os.Stderr, "subid rm: stage1 re-exec: %v\n", err)
		os.Exit(1)
	}
}

// isRootEscape reports an os.Root refusal of a path resolving outside the root:
// skipped, as the entry is not part of the image's own tree.
func isRootEscape(err error) bool {
	var pe *os.PathError
	return errors.As(err, &pe) && pe.Err != nil && pe.Err.Error() == "path escapes from parent"
}
