package sklet

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// The launcher is a copy of sklet that `sklet host setup` installs once, as
// root, with the network capabilities on it. The agent unit starts it; it hands
// those capabilities to ~/.local/bin/sklet as ambient capabilities and execs it.
// File capabilities live on an inode, so every replaced sklet binary lost them
// and needed sudo again; the launcher is never replaced, so an upgrade is a
// plain file swap by the node's own user.
const launcherName = "sklet-launcher"

var launcherCaps = []uintptr{unix.CAP_NET_BIND_SERVICE, unix.CAP_NET_ADMIN}

func LauncherPath(home string) string {
	return filepath.Join(home, ".local", "libexec", launcherName)
}

func AgentBinaryPath(home string) string {
	return filepath.Join(home, ".local", "bin", "sklet")
}

// IsLauncher reports whether this process runs from the launcher file. It
// reads /proc/self/exe, not argv[0], which the caller chooses freely.
func IsLauncher() bool {
	exe, err := os.Executable()
	return err == nil && filepath.Base(exe) == launcherName
}

// RunLauncher never returns on success: it becomes the agent binary.
func RunLauncher(args []string) int {
	runtime.LockOSThread()
	target, err := launcherTarget()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklet-launcher:", err)
		return 1
	}
	if err := raiseAmbient(launcherCaps); err != nil {
		fmt.Fprintln(os.Stderr, "sklet-launcher:", err)
		return 1
	}
	err = unix.Exec(target, append([]string{target}, args...), os.Environ())
	fmt.Fprintf(os.Stderr, "sklet-launcher: exec %s: %v\n", target, err)
	return 1
}

// launcherTarget is the invoking user's agent binary, found through the
// password database rather than $HOME, and refused unless only that user can
// have written it: whoever can replace it inherits the capabilities.
func launcherTarget() (string, error) {
	u, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		return "", err
	}
	target := AgentBinaryPath(u.HomeDir)
	if err := ownedOnlyBy(target, os.Getuid()); err != nil {
		return "", err
	}
	return target, nil
}

func ownedOnlyBy(path string, uid int) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != uid {
		return fmt.Errorf("%s is not owned by uid %d", path, uid)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by others than its owner", path)
	}
	return nil
}

// raiseAmbient moves each capability the launcher file granted into the
// inheritable and ambient sets, the only sets an exec of a capability-less
// binary keeps. Capabilities are per thread: the caller locked this one.
func raiseAmbient(caps []uintptr) error {
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		return fmt.Errorf("capget: %w", err)
	}
	for _, c := range caps {
		idx, bit := c/32, uint32(1)<<(c%32)
		if data[idx].Permitted&bit == 0 {
			return fmt.Errorf("capability %d missing from the launcher: re-run `sudo sklet host setup`", c)
		}
		data[idx].Inheritable |= bit
	}
	if err := unix.Capset(&hdr, &data[0]); err != nil {
		return fmt.Errorf("capset: %w", err)
	}
	for _, c := range caps {
		if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_RAISE, c, 0, 0); err != nil {
			return fmt.Errorf("raise ambient capability %d: %w", c, err)
		}
	}
	return nil
}

// LauncherReady reports whether the launcher is installed and still carries
// its capabilities (a chown or a copy silently strips them).
func LauncherReady(home string) bool {
	path := LauncherPath(home)
	if _, err := os.Stat(path); err != nil {
		return false
	}
	return skletCapsPresent(path)
}

// installLauncher copies the running sklet as the user's launcher and puts the
// capabilities on it. Root only: that is the one step that needs it.
func installLauncher(selfPath string, hu hostUser) error {
	u, err := user.Lookup(hu.name)
	if err != nil {
		return err
	}
	gid, _ := strconv.Atoi(u.Gid)
	dest := LauncherPath(u.HomeDir)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if err := os.Chown(filepath.Dir(dest), hu.uid, gid); err != nil {
		return err
	}
	tmp := dest + ".tmp"
	if err := copyFile(selfPath, tmp, 0o755); err != nil {
		return err
	}
	if err := os.Chown(tmp, hu.uid, gid); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	if err := applySkletCaps(tmp); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	return os.Rename(tmp, dest)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
