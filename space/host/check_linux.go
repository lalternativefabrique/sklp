//go:build linux

package host

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
)

// Problem is one thing the host lacks to run a Space. Fatal problems stop a
// Space before any work; the others are reported and the run continues.
type Problem struct {
	Name   string
	Detail string
	Fix    string
	Fatal  bool
}

func (p Problem) String() string {
	sev := "warn"
	if p.Fatal {
		sev = "fatal"
	}
	return fmt.Sprintf("%-5s %s: %s — %s", sev, p.Name, p.Detail, p.Fix)
}

// Check probes the running host for everything a Space needs and returns what
// is missing. cacheRoot is the sklp cache directory the Space would use; an
// empty string skips the filesystem probe.
func Check(cacheRoot string) []Problem {
	var out []Problem
	add := func(p Problem, missing bool) {
		if missing {
			out = append(out, p)
		}
	}

	_, err := os.Stat("/sys/fs/cgroup/cgroup.controllers")
	add(Problem{"cgroups", "cgroups v2 is not mounted at /sys/fs/cgroup", "boot with systemd.unified_cgroup_hierarchy=1 or mount cgroup2", true}, err != nil)

	for _, bin := range []string{"nft", "git", "unshare"} {
		_, err := exec.LookPath(bin)
		add(Problem{bin, bin + " is not on PATH", "install " + packageFor(bin), true}, err != nil)
	}
	if out, err := exec.Command("unshare", "--help").CombinedOutput(); err == nil && !strings.Contains(string(out), "map-users") {
		add(Problem{"unshare", "unshare is the BusyBox applet, which has no --map-users", "install util-linux", true}, true)
	}

	if uid := os.Getuid(); uid != 0 {
		_, err := exec.LookPath("newuidmap")
		add(Problem{"newuidmap", "newuidmap/newgidmap are not on PATH, so no subuid range can be mapped", "install uidmap", true}, err != nil)
		add(Problem{"subuid", fmt.Sprintf("no /etc/subuid range for uid %d", uid), "add <user>:100000:65536 to /etc/subuid and /etc/subgid", true}, !hasSubidRange(uid))
	} else {
		_, err := exec.LookPath("newuidmap")
		add(Problem{"newuidmap", "running as root without newuidmap: Spaces map a single uid, images that chown to a service uid will fail", "install uidmap and add root:100000:65536 to /etc/subuid and /etc/subgid", false}, err != nil || !hasSubidRange(0))
	}

	if f, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0); err != nil {
		add(Problem{"tun", "/dev/net/tun cannot be opened: " + err.Error(), "load the tun module, or in a pod expose the device (mknod /dev/net/tun c 10 200 needs CAP_MKNOD)", true}, true)
	} else {
		f.Close()
	}

	if hidden := procSubmounts(); len(hidden) > 0 {
		add(Problem{"proc", "entries are mounted over /proc (" + strings.Join(hidden, ", ") + "); the kernel refuses a nested proc mount over hidden entries", "run outside a masked container, or with procMount: Unmasked / hostUsers: false on Kubernetes", true}, true)
	}

	if cacheRoot != "" {
		add(Problem{"cache", cacheRoot + " is on overlayfs, which cannot host an overlay upperdir", "point XDG_CACHE_HOME at a volume on a real filesystem (ext4, xfs, btrfs)", true}, onOverlayfs(cacheRoot))
	}
	return out
}

// onOverlayfs probes the nearest existing ancestor, so a cache root that is not
// created yet is judged by the filesystem it would land on.
func onOverlayfs(path string) bool {
	for p := path; p != "/" && p != "."; p = filepath.Dir(p) {
		var st syscall.Statfs_t
		if err := syscall.Statfs(p, &st); err == nil {
			return st.Type == overlayfsMagic
		}
	}
	return false
}

// HasFatal reports whether any problem stops a Space from running.
func HasFatal(problems []Problem) bool {
	for _, p := range problems {
		if p.Fatal {
			return true
		}
	}
	return false
}

const overlayfsMagic = 0x794c7630

func packageFor(bin string) string {
	switch bin {
	case "nft":
		return "nftables"
	case "unshare":
		return "util-linux"
	}
	return bin
}

func hasSubidRange(uid int) bool {
	u, err := user.LookupId(fmt.Sprint(uid))
	if err != nil {
		return false
	}
	f, err := os.Open("/etc/subuid")
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, _, ok := strings.Cut(sc.Text(), ":")
		if ok && (name == u.Username || name == fmt.Sprint(uid)) {
			return true
		}
	}
	return false
}

func procSubmounts() []string {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	defer f.Close()
	var hidden []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 || !strings.HasPrefix(fields[4], "/proc/") {
			continue
		}
		// binfmt_misc (and systemd's autofs in front of it) sits on a mount
		// point the kernel created permanently empty, the one kind of /proc
		// child a nested proc mount may hide.
		if fields[4] == "/proc/sys/fs/binfmt_misc" {
			continue
		}
		hidden = append(hidden, fields[4])
	}
	return hidden
}
