//go:build linux

package space

import (
	"fmt"
	"github.com/lalternativefabrique/sklp/space/ociproc"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// waitProcessGone polls until pid no longer exists (signal 0 errors) or timeout.
// The holder is a child of the space worker, not of this process, so it cannot be
// reaped with Wait — kill(pid, 0) is the portable liveness check. Best-effort: on
// timeout it returns anyway (the subsequent mount surfaces any real EBUSY).
func waitProcessGone(pid int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return // gone (ESRCH) or not ours (EPERM) — either way no longer holding the mount
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// serviceOverlay is a per-service writable rootfs: an overlayfs whose lowerdir
// is the shared read-only digest rootfs and whose upperdir is per-service
// scratch. mergedPath is the runc root.path; holderPID is the overlay holder
// whose mount-ns runc is entered into.
type serviceOverlay struct {
	mergedPath string
	holderPID  int
}

// overlayDir is where a space materialises its per-service overlay scratch.
// Per-space + per-service so two services (or two spaces) never share an upper.
func (s spaceScope) overlayDir(svc string) string {
	return filepath.Join(s.spaceDir(), "overlay", sanitize(svc))
}

// PurgeSpaceScratch removes a space's ephemeral overlay scratch
// (<spaceDir>/overlay/*: per-service upper/work/merged). It is the teardown
// counterpart of setupServiceOverlay: the overlay holder dies with the space's
// worker, so by the time down runs the overlay is already unmounted; this just
// reclaims the discarded upper (which IS the immutability guarantee for the next
// run). The upper is owned by mapped subuids when a container ran as non-root,
// so a plain remove can hit EPERM — use the subuid-aware removal. Named volumes
// (persistent) live elsewhere and are untouched. Best-effort by contract; a
// leftover only wastes disk.
func PurgeSpaceScratch(space string) error {
	root, err := cacheRoot()
	if err != nil {
		return nil // no cache root → nothing materialised
	}
	// <spaceDir>/overlay — the parent of every per-service scratch dir. Removed
	// whole; named volumes live under <spaceDir>/volumes and are untouched.
	dir := filepath.Join(newScope(space, root).spaceDir(), "overlay")
	if _, err := os.Stat(dir); err != nil {
		return nil // nothing to purge
	}
	return subidRemoveAll(dir)
}

// PurgeSpaceCache removes a space's ENTIRE cache dir (<spaceDir>: overlay
// scratch, deploy-bundles, runc-state — everything PurgeSpaceScratch leaves
// behind), not just the overlay scratch. This is what `space.Registry.Down`
// (space_registry.go) has always done for `sklp space down -v`; `sklet.Down`
// (the `sklp deploy delete` path) only ever called PurgeSpaceScratch, leaving
// deploy-bundles/runc-state as a permanent ~40K/space leak on every remote
// delete — found live on node-a, 2026-08-30.
//
// keepVolumes controls whether named volumes are preserved, mirroring `space
// down` (keeps them) vs `space down -v`/`purge` (drops them): when volumes
// live UNDER spaceDir (no data root configured — the legacy cache layout),
// removing spaceDir whole would take them with it, so this walks spaceDir's
// immediate children and skips the "volumes" entry instead of one
// subidRemoveAll on the whole tree. When a data root IS configured, volumes
// already live outside spaceDir (volumesRoot's other branch) and this is
// equivalent to removing spaceDir whole — the per-entry walk is then just a
// slightly more expensive no-op difference.
//
// `sklp deploy delete` has no `-v` equivalent today and has never dropped
// named volumes, so its caller always passes keepVolumes=true — this
// preserves that existing behaviour exactly, it does not change it.
func PurgeSpaceCache(space string, keepVolumes bool) error {
	root, err := cacheRoot()
	if err != nil {
		return nil // no cache root → nothing materialised
	}
	scope := newScope(space, root)
	dir := scope.spaceDir()
	if _, err := os.Stat(dir); err != nil {
		return nil // nothing to purge
	}
	if !keepVolumes {
		if err := subidRemoveAll(dir); err != nil {
			return err
		}
		if dataSpaceDir := filepath.Dir(scope.volumesRoot()); !scope.standalone() && dataSpaceDir != dir {
			return subidRemoveAll(dataSpaceDir)
		}
		return nil
	}
	volumes := filepath.Base(scope.volumesRoot())
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("purge space cache for %q: %w", space, err)
	}
	for _, e := range entries {
		if e.Name() == volumes && filepath.Dir(scope.volumesRoot()) == dir {
			continue // named volumes live here (no data root) — preserve them
		}
		if err := subidRemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return fmt.Errorf("purge space cache for %q: remove %s: %w", space, e.Name(), err)
		}
	}
	// With a data root the volumes never lived here, so the loop just emptied
	// the dir — and the space inventory lists NAMES (dir entries), so an empty
	// leftover reads as a ghost space forever. Remove succeeds only when
	// empty; kept in-dir volumes make it fail, which is exactly the intent.
	_ = os.Remove(dir)
	return nil
}

// lowerMarkerName records which image rootfs an upper was written over, so the
// next launch can tell a restart on the same image from a roll onto a new one.
const lowerMarkerName = ".lower"

// discardStaleUpper drops the scratch when it was written over another image.
// A missing marker is treated as stale: an upper from before this file existed
// has unknown provenance, and the cost of being wrong is one cold start.
func discardStaleUpper(base, lowerRootfs string) error {
	if _, err := os.Stat(base); err != nil {
		return nil // nothing materialised yet
	}
	if prev, err := os.ReadFile(filepath.Join(base, lowerMarkerName)); err == nil {
		if string(prev) == lowerRootfs {
			return nil
		}
	}
	// Owned by mapped subuids whenever the container ran as non-root, so a
	// plain RemoveAll hits EPERM.
	if err := subidRemoveAll(base); err != nil {
		return fmt.Errorf("discard stale overlay %q: %w", base, err)
	}
	return nil
}

// setupServiceOverlay prepares the per-service overlay dirs and asks the space
// worker to spawn a holder that mounts the overlay in its own mount-ns. It
// returns the merged mountpoint (runc root.path) and the holder pid (runc is
// nsenter'd into it). lowerRootfs is the shared extracted digest rootfs.
//
// upper/work/merged live on the same filesystem (siblings) — an overlayfs
// requirement. The upper is ephemeral: discarded at teardown (the immutability
// guarantee). The holder dies with the worker, unmounting the overlay.
func (s *Space) setupServiceOverlay(cnet *spaceNet, svcName, lowerRootfs string) (serviceOverlay, error) {
	// Idempotency across restarts: the supervisor calls Start again when a service
	// crashes. A prior holder still holds an overlay over THIS service's
	// upper/work, so a fresh mount with the same upper/work returns EBUSY. Kill
	// the old holder first — its overlay (and mount-ns) goes with it — so the
	// relaunch mounts cleanly. SIGKILL: the holder only parks, no cleanup to run.
	s.releaseOverlayHolder(cnet, svcName)

	base := s.scope.overlayDir(svcName)
	// An upper written over a DIFFERENT image has to go. It shadows the new
	// lower file for file, so a container keeps serving what the previous
	// image's entrypoint wrote — chat's ui served an nginx config from two
	// images back through nine rollouts, each one pulling the right image and
	// then hiding it. Discarding the upper is the immutability guarantee this
	// file states twice; it was only ever honoured at teardown.
	if err := discardStaleUpper(base, lowerRootfs); err != nil {
		return serviceOverlay{}, err
	}
	upper := filepath.Join(base, "upper")
	work := filepath.Join(base, "work")
	merged := filepath.Join(base, "merged")
	for _, d := range []string{upper, work, merged} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return serviceOverlay{}, fmt.Errorf("overlay dir %q: %w", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, lowerMarkerName), []byte(lowerRootfs), 0o644); err != nil {
		return serviceOverlay{}, fmt.Errorf("overlay lower marker: %w", err)
	}
	var pid int
	var err error
	if s.childUserns {
		// The holder runs as the space's root, a subuid: it must pass through
		// to this binary, which it execs, and to the cache root before it can
		// map itself. A login home is 0750 on Ubuntu, so without this the
		// holder dies before writing its gid_map and no service starts.
		if err := s.openPathsToChild(); err != nil {
			return serviceOverlay{}, fmt.Errorf("open paths to the child root: %w", err)
		}
		slot, serr := childRootSlot(s.cacheRoot, s.scope.name)
		if serr != nil {
			return serviceOverlay{}, serr
		}
		childLower, lerr := s.childLowerFor(cnet, lowerRootfs, slot)
		if lerr != nil {
			return serviceOverlay{}, lerr
		}
		root, ok := cnet.identity().SpaceRootOf(slot, childRootSlots)
		if !ok {
			return serviceOverlay{}, fmt.Errorf("child root slot %d does not fit the delegated range", slot)
		}
		if err := handOverlayDirsToChild(root.HostUID, upper, work, merged, func(paths ...string) error {
			return cnet.handToChild(root.WorkerID, paths...)
		}); err != nil {
			return serviceOverlay{}, fmt.Errorf("hand overlay dirs to the child root: %w", err)
		}
		var ident ociproc.Identity
		pid, ident, err = cnet.spawnOverlayChild(childLower, upper, work, merged, slot)
		if err == nil {
			err = s.adoptChildIdentity(cnet, ident)
		}
	} else {
		pid, err = cnet.spawnOverlay(lowerRootfs, upper, work, merged)
	}
	if err != nil {
		return serviceOverlay{}, fmt.Errorf("spawn overlay holder: %w", err)
	}
	s.mu.Lock()
	s.overlayHolders[svcName] = pid
	s.mu.Unlock()
	return serviceOverlay{mergedPath: merged, holderPID: pid}, nil
}

// adoptChildIdentity records the child identity the space's services run as
// and, once, prepares what runc needs to run as that root: every runc call on
// this state root is entered as the space root (who can read and signal the
// child's containers), and the state root itself is handed to the child so
// runc can write its state there.
func (s *Space) adoptChildIdentity(cnet *spaceNet, ident ociproc.Identity) error {
	s.mu.Lock()
	first := s.childRootID == 0
	s.childIdent = ident
	s.childRootID = cnet.identity().WorkerIDOf(ident.HostUID)
	rootID := s.childRootID
	s.mu.Unlock()
	if first {
		setRuncEnter(s.runcRoot, cnet.enterAsSpaceRoot())
	}
	// A deleted space took its state root with it; the agent's Space value
	// outlives that, so the root is made and handed again whenever it is
	// not the child's.
	if err := os.MkdirAll(s.runcRoot, 0o755); err != nil {
		return err
	}
	owned, err := ownedBy(s.runcRoot, ident.HostUID)
	if err != nil {
		return err
	}
	if owned {
		return nil
	}
	if err := cnet.handToChild(rootID, s.runcRoot); err != nil {
		return fmt.Errorf("hand runc state root to the child root: %w", err)
	}
	return nil
}

// childLowerFor is this space's own copy of the digest rootfs, owned by its
// child root. Files the operator unpacked belong to an id the child maps to
// nobody, so the container's root could write none of them (runc's /dev
// nodes, an entrypoint's chown), and a copy-up over them fails the same way;
// a file belongs to one uid, and every space has its own root, so every
// space gets its own copy. Made once per digest, by the space root, who
// alone may chown into the range: copied beside its final name, then renamed.
func (s *Space) childLowerFor(cnet *spaceNet, digestRootfs string, slot int) (string, error) {
	root, ok := cnet.identity().SpaceRootOf(slot, childRootSlots)
	if !ok {
		return "", fmt.Errorf("child root slot %d does not fit the delegated range", slot)
	}
	rootID, hostRoot := root.WorkerID, root.HostUID
	dst := filepath.Join(s.scope.childRootfsDir(), filepath.Base(digestRootfs))
	if st, err := os.Stat(dst); err == nil {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) == hostRoot {
			return dst, nil
		}
		if err := cnet.runAsSpaceRoot("rm", "-rf", dst); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o711); err != nil {
		return "", err
	}
	tmp := dst + ".copying"
	_ = cnet.runAsSpaceRoot("rm", "-rf", tmp)
	if err := cnet.runAsSpaceRoot("cp", "-a", digestRootfs, tmp); err != nil {
		return "", fmt.Errorf("copy rootfs for the child root: %w", err)
	}
	// Only what container root owns becomes the space root's: files an image
	// gave another user keep that user, whom the child maps like the host.
	id0 := strconv.Itoa(rootID)
	if err := cnet.runAsSpaceRoot("find", tmp, "-user", "0", "-exec", "chown", "-h", id0, "{}", "+"); err != nil {
		return "", fmt.Errorf("hand rootfs copy to the child root: %w", err)
	}
	if err := cnet.runAsSpaceRoot("find", tmp, "-group", "0", "-exec", "chgrp", "-h", id0, "{}", "+"); err != nil {
		return "", fmt.Errorf("hand rootfs copy to the child root: %w", err)
	}
	if err := cnet.runAsSpaceRoot("mv", tmp, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// handOverlayDirsToChild gives the overlay directories to the space's root
// once, when they are born. An upper the child root already owns is left as
// it is: the files a service wrote there belong to the ids it wrote them as,
// and chowning them again to root at every start is how a restarted mariadb
// found its own pid file no longer writable. An upper some other owner
// populated (a run before this space went child) is discarded, as an upper
// over another image is: it is ephemeral by contract.
func handOverlayDirsToChild(rootHostUID int, upper, work, merged string, hand func(paths ...string) error) error {
	owned, err := ownedBy(upper, rootHostUID)
	if err != nil {
		return err
	}
	if owned {
		return nil
	}
	for _, d := range []string{upper, work} {
		entries, err := os.ReadDir(d)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			continue
		}
		if err := os.RemoveAll(d); err != nil {
			return err
		}
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return hand(upper, work, merged)
}

func ownedBy(path string, uid int) (bool, error) {
	st, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	return ok && int(sys.Uid) == uid, nil
}

// releaseOverlayHolder kills the process parking a service instance's overlay
// and waits for it to exit: the overlay, and the kernel's hold on upper/work,
// only go when its mount-ns is torn down. A stopped instance whose holder
// survives pins its image, and a node rolling images leaks one per rollout.
func (s *Space) releaseOverlayHolder(cnet *spaceNet, name string) {
	s.mu.Lock()
	pid := s.overlayHolders[name]
	delete(s.overlayHolders, name)
	s.mu.Unlock()
	if pid <= 0 {
		return
	}
	if s.childUserns && cnet != nil {
		_ = cnet.killAsSpaceRoot(pid)
	} else {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	waitProcessGone(pid, 2*time.Second)
}
