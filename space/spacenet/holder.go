//go:build linux

package spacenet

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// holderEnv marks a re-exec'd process that should just park, holding its
// (private) network namespace alive. It is spawned BY THE WORKER, so it
// inherits the worker's user namespace — giving a netns the worker has
// CAP_NET_ADMIN over, exactly the ownership Attach needs.
const holderEnv = "SKLP_SPACE_HOLDER"

// spawnNetnsHolder forks a child that enters a fresh network namespace
// (CLONE_NEWNET only — no new userns, so it stays in the worker's userns) and
// parks. It returns the child's host-visible pid; /proc/<pid>/ns/net is then a
// netns the worker owns and can attach to the bridge. The child is reaped when
// the worker exits (parent death) — it is a space-lifetime peer.
//
// This is the primitive a real space container reuses: a runc container that
// joins the space userns (UsernsPath) but keeps its own netns presents the
// same ownership shape. The simulated holder lets the network be validated
// without runc.
func spawnNetnsHolder() (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("holder: resolve self: %w", err)
	}
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), holderEnv+"=1")
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWNET, // own netns, SAME userns as the worker
		Pdeathsig:  syscall.SIGKILL,      // die with the worker, no leak
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("holder: start: %w", err)
	}
	reapWhenDone(cmd)
	return cmd.Process.Pid, nil
}

// park blocks forever; the holder is killed via Pdeathsig when the worker
// exits, or explicitly by the host during teardown. It blocks on a signal wait
// rather than `select {}`: an empty select is the ONLY runnable goroutine's sole
// statement, so Go's deadlock detector fatals it ("all goroutines are asleep")
// once GC/sysmon go idle — a non-deterministic crash that left the holder's
// mount/net ns gone and runc's nsenter failing. A signal channel keeps a
// goroutine genuinely blocked on the runtime's signal path (never flagged as a
// deadlock); the holder still dies via Pdeathsig/teardown as before.
//
// It also exits on its own once reparented to init: a holder is a child of the
// worker and has no purpose without it. PR_SET_PDEATHSIG covers the ordinary
// case, but the kernel clears it whenever the process gains capabilities, and
// an orphan that runs as a subuid can neither be seen (its environ is
// unreadable) nor killed by the operator. Polling the parent closes that gap.
func park() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ch:
			os.Exit(0)
		case <-tick.C:
			if os.Getppid() == 1 {
				os.Exit(0)
			}
		}
	}
}

// Overlay holder: the mount-ns twin of the netns holder. It is spawned BY THE
// WORKER (inherits the worker userns), unshares its OWN mount namespace
// (CLONE_NEWNS), mounts a per-service overlayfs whose lowerdir is the shared
// read-only digest rootfs and whose upperdir is per-service scratch, then parks.
// sklp later `nsenter -U --mount`s runc into this holder so runc's rootfs is the
// merged overlay (writable) while the digest lower stays pristine and shared.
//
// One holder PER SERVICE — never shared: two runc containers in one mount-ns
// collide at pivot_root (proven). The holder dies with the worker (Pdeathsig),
// which destroys its mount-ns and unmounts the overlay automatically.
const (
	overlayEnv    = "SKLP_SPACE_OVERLAY"       // marks an overlay-holder re-exec
	overlayLower  = "SKLP_SPACE_OVERLAY_LOWER" // lowerdir: shared digest rootfs (RO)
	overlayUpper  = "SKLP_SPACE_OVERLAY_UPPER" // upperdir: per-service scratch (RW)
	overlayWork   = "SKLP_SPACE_OVERLAY_WORK"  // overlayfs workdir (sibling of upper)
	overlayMerged = "SKLP_SPACE_OVERLAY_MERGED"
	// overlayChildEnv marks a holder that owns a CHILD user namespace of the
	// worker's: "1" while parked on the sync pipe for its maps, "2" once
	// re-exec'd with the namespace capabilities.
	overlayChildEnv = "SKLP_SPACE_OVERLAY_CHILD"
)

// ChildRootSlots is how many ids at the top of the delegated range are set
// aside as space roots. No child maps any of them but its own, as 0: a
// space's root is nobody inside every other space.
const ChildRootSlots = 4096

// childUserns is the mapping a child-userns holder gets, expressed in the
// worker's ids: container 0 -> RootID (a subuid that owns nothing on the
// host), container 1..Count -> the same ids as the worker maps them.
type childUserns struct{ RootID, Count int }

func (c childUserns) idMap() string {
	return fmt.Sprintf("0 %d 1\n1 1 %d\n", c.RootID, c.Count)
}

// writeMaps writes the child's maps. The worker is uid 0 of the parent
// namespace with CAP_SETUID/CAP_SETGID there, and the ids are a slice of its
// own, which is all a direct write to /proc needs: no setuid helper, and no
// setgroups=deny either (that is only demanded of a writer without CAP_SETGID
// in the parent), so the child's root keeps setgroups for nsenter -G and runc.
func (c childUserns) writeMaps(pid int) error {
	proc := fmt.Sprintf("/proc/%d/", pid)
	if err := os.WriteFile(proc+"gid_map", []byte(c.idMap()), 0); err != nil {
		return fmt.Errorf("child userns: gid_map: %w", err)
	}
	if err := os.WriteFile(proc+"uid_map", []byte(c.idMap()), 0); err != nil {
		return fmt.Errorf("child userns: uid_map: %w", err)
	}
	return nil
}

// spawnOverlayHolder forks a holder that unshares a mount-ns, mounts the
// service overlay in it, and parks. It returns the holder's host-visible pid;
// /proc/<pid>/ns/{user,mnt} is then the namespace pair sklp enters runc into.
// lower/upper/work/merged must already exist on disk (the host MkdirAll's them).
//
// With child set the holder is cloned into its OWN user namespace, a child of
// the worker's, mapped by the worker (childUserns.writeMaps) while parked on the
// sync pipe; it re-execs once mapped so the mount runs with that namespace's
// capabilities. upper, work and merged are handed to the child's root first:
// overlayfs writes the upper with the mounter's credentials, and the child's
// root is a subuid the worker (not the host) may chown to.
func spawnOverlayHolder(lower, upper, work, merged string, child *childUserns) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("overlay holder: resolve self: %w", err)
	}
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(),
		overlayEnv+"=1",
		overlayLower+"="+lower,
		overlayUpper+"="+upper,
		overlayWork+"="+work,
		overlayMerged+"="+merged,
	)
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWNS, // own mount-ns; the worker's userns unless child
		Pdeathsig:  syscall.SIGKILL,     // die with the worker → overlay unmounts
	}
	if child == nil {
		if err := cmd.Start(); err != nil {
			return 0, fmt.Errorf("overlay holder: start: %w", err)
		}
		reapWhenDone(cmd)
		return awaitOverlayMounted(cmd, merged)
	}
	for _, d := range []string{upper, work, merged} {
		if err := os.Chown(d, child.RootID, child.RootID); err != nil {
			return 0, fmt.Errorf("overlay holder: hand %q to child root %d: %w", d, child.RootID, err)
		}
	}
	cmd.Env = append(cmd.Env, overlayChildEnv+"=1")
	if err := startChildUserns(cmd, child); err != nil {
		return 0, fmt.Errorf("overlay holder (child userns): %w", err)
	}
	reapWhenDone(cmd)
	return awaitOverlayMounted(cmd, merged)
}

// awaitOverlayMounted returns the holder's pid once its mount namespace holds
// the overlay at merged. Without this wait runc is entered into the holder
// while the mount is still pending (EBUSY retries after the previous holder's
// death, the child-userns re-exec) and pivots onto an empty merged: the
// entrypoint is then "not found" and the launch fails once before the retry
// lands on the mounted overlay.
func awaitOverlayMounted(cmd *exec.Cmd, merged string) (int, error) {
	pid := cmd.Process.Pid
	deadline := time.Now().Add(overlayMountTimeout)
	for {
		blob, err := os.ReadFile(fmt.Sprintf("/proc/%d/mountinfo", pid))
		if err != nil {
			return 0, fmt.Errorf("overlay holder %d exited before mounting %s", pid, merged)
		}
		if overlayMountedAt(blob, merged) {
			return pid, nil
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			return 0, fmt.Errorf("overlay holder %d: %s not mounted after %s", pid, merged, overlayMountTimeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// overlayMountTimeout covers the holder's own EBUSY retries (up to 1s) and the
// child-userns map-then-re-exec round trip.
const overlayMountTimeout = 5 * time.Second

// overlayMountedAt reports whether a mountinfo listing has an overlay mounted
// exactly at merged. Field 5 is the mount point, the fs type follows the "-"
// separator; mountinfo escapes spaces in paths as \040.
func overlayMountedAt(mountinfo []byte, merged string) bool {
	want := strings.ReplaceAll(merged, " ", "\\040")
	for _, line := range strings.Split(string(mountinfo), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[4] != want {
			continue
		}
		for i, f := range fields {
			if f == "-" && i+1 < len(fields) {
				if fields[i+1] == "overlay" {
					return true
				}
				break
			}
		}
	}
	return false
}

// startChildUserns starts cmd as the child's root inside a fresh user+mount
// namespace pair, parked on the sync pipe until the worker has written its maps.
//
// Capabilities in a user namespace are granted at execve only to uid 0 OF that
// namespace, so the process must already run as the id that becomes the child's
// root (RootID, a mapped id the worker may setuid to) BEFORE the namespace is
// created — a clone-time CLONE_NEWUSER would keep the worker's uid, unmapped in
// the child, and the holder would exec as nobody with no capability. And a Go
// process cannot unshare(CLONE_NEWUSER) itself once its runtime is threaded, so
// the unshare is done by unshare(1), single-threaded, which then execs cmd.
// Propagation is left unchanged there: at that point the namespace is unmapped
// and the private remount runOverlayHolder does would be refused.
func startChildUserns(cmd *exec.Cmd, child *childUserns) error {
	unshareBin, err := exec.LookPath("unshare")
	if err != nil {
		return fmt.Errorf("unshare(1) is required for a child user namespace: %w", err)
	}
	cmd.Args = append([]string{unshareBin, "--user", "--mount", "--propagation", "unchanged", "--", cmd.Path}, cmd.Args[1:]...)
	cmd.Path = unshareBin
	cmd.SysProcAttr.Cloneflags = 0
	// Groups: none. The worker's supplementary groups are meaningless in the
	// child and, carried along, make the overlay mount fail with EACCES.
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(child.RootID), Gid: uint32(child.RootID), Groups: []uint32{}}

	pr, pw, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("sync pipe: %w", err)
	}
	cmd.ExtraFiles = append(cmd.ExtraFiles, pr)
	cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%d", envSyncFD, 3+len(cmd.ExtraFiles)-1))
	if err := startPinned(cmd); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return err
	}
	_ = pr.Close()

	mapErr := waitForeignUserns(cmd.Process.Pid, 5*time.Second)
	if mapErr == nil {
		mapErr = writeMapsWhenDumpable(cmd.Process.Pid, child, 5*time.Second)
	}
	_ = pw.Close()
	if mapErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return mapErr
	}
	return nil
}

// writeMapsWhenDumpable writes the child's maps, retrying while the write is
// refused: between unshare(1)'s exec of the holder and the holder's own
// PR_SET_DUMPABLE the map files belong to root for a moment.
func writeMapsWhenDumpable(pid int, child *childUserns, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		err := child.writeMaps(pid)
		if err == nil || !errors.Is(err, os.ErrPermission) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForeignUserns returns once pid lives in a user namespace other than the
// caller's, i.e. unshare(1) has run; it fails if pid exits first.
func waitForeignUserns(pid int, timeout time.Duration) error {
	own, err := os.Readlink("/proc/self/ns/user")
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		theirs, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/user", pid))
		if err != nil {
			return fmt.Errorf("child %d gone before unsharing: %w", pid, err)
		}
		if theirs != own {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fmt.Errorf("child %d never unshared its user namespace", pid)
}

// runOverlayChildStage1 is the parked half of a child-userns holder: it waits
// for the worker to write its maps, then re-execs itself so the mount in
// runOverlayHolder runs with the namespace capabilities execve grants.
func runOverlayChildStage1() {
	// Executing a binary that carries file capabilities (sklet does) makes the
	// process non-dumpable, which hands its /proc/<pid>/uid_map to root: the
	// worker could no longer write it. Declare ourselves dumpable again first.
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 1, 0); errno != 0 {
		fmt.Fprintf(os.Stderr, "overlay holder: set dumpable: %v\n", errno)
		os.Exit(1)
	}
	waitSubidMapping()
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "overlay holder: stage1 resolve self: %v\n", err)
		os.Exit(1)
	}
	env := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, envSyncFD+"=") || strings.HasPrefix(kv, overlayChildEnv+"=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, overlayChildEnv+"=2")
	if err := syscall.Exec(self, os.Args, env); err != nil {
		fmt.Fprintf(os.Stderr, "overlay holder: stage1 re-exec: %v\n", err)
		os.Exit(1)
	}
}

// runOverlayHolder is the holder side (already in its own mount-ns via the
// CLONE_NEWNS at spawn). It makes the mount-ns private so its overlay does not
// propagate to peers, mounts the overlay, then parks. It never returns. Called
// from MaybeRunWorker when overlayEnv is set.
func runOverlayHolder() {
	rearmParentDeath()
	lower := os.Getenv(overlayLower)
	upper := os.Getenv(overlayUpper)
	work := os.Getenv(overlayWork)
	merged := os.Getenv(overlayMerged)

	// Private propagation: a per-service mount must not leak into sibling holders.
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		fmt.Fprintf(os.Stderr, "overlay holder: make-rprivate /: %v\n", err)
		os.Exit(1)
	}
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lower, upper, work)
	// Retry briefly on EBUSY: a prior holder for this service was just killed, and
	// the kernel releases its hold on upper/work only when that holder's mount-ns
	// is fully torn down — which can lag the process death by a few ms. The host
	// already waited for the old pid to exit; this absorbs the residual window.
	var err error
	for i := 0; i < 50; i++ {
		err = syscall.Mount("overlay", merged, "overlay", 0, opts)
		if err == nil {
			break
		}
		if err != syscall.EBUSY {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "overlay holder: mount overlay at %s: %v\n", merged, err)
		os.Exit(1)
	}
	park() // killed via Pdeathsig when the worker exits
}

// rearmParentDeath re-installs the die-with-the-worker signal. The kernel
// clears PR_SET_PDEATHSIG whenever a process gains capabilities it did not
// have (commit_creds), which a child-userns holder does twice: at unshare and
// at its mapped re-exec. A holder whose worker is already gone exits now.
func rearmParentDeath() {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_PDEATHSIG, uintptr(syscall.SIGKILL), 0); errno != 0 {
		fmt.Fprintf(os.Stderr, "overlay holder: rearm pdeathsig: %v\n", errno)
		os.Exit(1)
	}
	if os.Getppid() == 1 {
		os.Exit(0)
	}
}

// reapWhenDone collects the holder's exit status whenever it dies. The worker
// starts holders and forgets them, so a holder killed at the next rollout of
// its service would otherwise stay a zombie of the worker forever; each one
// counts against the unit's TasksMax until fork itself fails with EAGAIN
// (2237 zombies on node-e, 2026-09-22).
func reapWhenDone(cmd *exec.Cmd) {
	go func() {
		err := cmd.Wait()
		if cmd.ProcessState != nil && !cmd.ProcessState.Success() {
			fmt.Fprintf(os.Stderr, "holder %d exited: %v (%s)\n", cmd.ProcessState.Pid(), err, cmd.ProcessState.String())
		}
	}()
}
