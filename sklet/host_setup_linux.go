//go:build linux

package sklet

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/lalternativefabrique/sklp/internal/xdg"
)

// host_setup_linux.go implements `sklet host setup`: it makes a Linux host ready
// to run rootless spaces. The rootless model needs ONE root-time bootstrap that
// sklp itself cannot do unprivileged — the setuid helpers newuidmap/newgidmap
// (package uidmap) and the per-user subuid/subgid grant. So this command has two
// modes: run UNPRIVILEGED it only DIAGNOSES (prints the exact sudo commands to
// run); run UNDER ROOT (sudo) it APPLIES the fixes for the invoking user
// ($SUDO_USER), idempotently. Everything sklp CAN do rootless (read the subuid
// range, enable linger) stays in the normal deploy path; this is only the
// root-gated bootstrap.

// hostUser is the user the prerequisites must target. Under sudo we configure
// the INVOKING user (SUDO_USER), not root — root running no spaces is useless.
type hostUser struct {
	name string
	uid  int
}

func resolveHostUser() (hostUser, error) {
	name := os.Getenv("SUDO_USER")
	if name == "" {
		u, err := user.Current()
		if err != nil {
			return hostUser{}, err
		}
		name = u.Username
	}
	u, err := user.Lookup(name)
	if err != nil {
		return hostUser{}, fmt.Errorf("resolve user %q: %w", name, err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	return hostUser{name: name, uid: uid}, nil
}

// runHostSetup runs every check, then either applies (root) or prints the sudo
// commands (unprivileged). Returns 0 when the host is ready (or was made ready),
// 1 when a prerequisite is still missing after a non-root run.
func RunHostSetup() int {
	root := os.Geteuid() == 0
	hu, err := resolveHostUser()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklet host setup:", err)
		return 1
	}

	mode := "check"
	if root {
		mode = "apply"
	}
	fmt.Printf("sklet host setup (%s) for user %q\n\n", mode, hu.name)

	var todo []string // sudo commands the user must still run (unprivileged mode)
	ok := true

	// 1. uidmap (newuidmap/newgidmap) — REQUIRED, root to install.
	if hasUIDMap() {
		fmt.Println("  ✓ uidmap (newuidmap/newgidmap) present")
	} else {
		ok = false
		if root {
			if err := installUIDMap(); err != nil {
				fmt.Printf("  ✗ uidmap: install failed: %v\n", err)
			} else {
				fmt.Println("  ✓ uidmap installed")
				ok = true
			}
		} else {
			fmt.Println("  ✗ uidmap MISSING")
			todo = append(todo, uidmapInstallCmd())
		}
	}

	// 2. subuid/subgid range — REQUIRED, root to grant. Any valid range works
	//    (sklp reads the real base; it is not pinned to 100000).
	subuidOK := hasSubIDRange("/etc/subuid", hu)
	subgidOK := hasSubIDRange("/etc/subgid", hu)
	if subuidOK && subgidOK {
		fmt.Printf("  ✓ subuid/subgid range allocated to %q\n", hu.name)
	} else {
		ok = false
		if root {
			if err := grantSubIDRange(hu); err != nil {
				fmt.Printf("  ✗ subuid/subgid: grant failed: %v\n", err)
			} else {
				fmt.Printf("  ✓ subuid/subgid range granted to %q\n", hu.name)
				ok = true
			}
		} else {
			fmt.Printf("  ✗ subuid/subgid range MISSING for %q\n", hu.name)
			todo = append(todo, fmt.Sprintf("sudo usermod --add-subuids 100000-165535 --add-subgids 100000-165535 %s", hu.name))
		}
	}

	// 3. lingering — services survive logout/reboot. Root to enable.
	if lingerEnabled(hu.name) {
		fmt.Printf("  ✓ lingering enabled for %q\n", hu.name)
	} else {
		if root {
			if err := enableLinger(hu.name); err != nil {
				fmt.Printf("  ! lingering: enable failed (spaces won't survive a reboot): %v\n", err)
			} else {
				fmt.Printf("  ✓ lingering enabled for %q\n", hu.name)
			}
		} else {
			fmt.Printf("  ! lingering NOT enabled for %q (spaces won't survive a reboot)\n", hu.name)
			todo = append(todo, fmt.Sprintf("sudo loginctl enable-linger %s", hu.name))
		}
	}

	// 4. unprivileged userns clone — read-only check (Debian/Ubuntu sysctl;
	//    absent elsewhere = enabled by default).
	switch unprivUsernsClone() {
	case "1", "":
		fmt.Println("  ✓ unprivileged user namespaces enabled")
	default:
		ok = false
		fmt.Println("  ✗ unprivileged user namespaces DISABLED (kernel.unprivileged_userns_clone=0)")
		if !root {
			todo = append(todo, "sudo sysctl -w kernel.unprivileged_userns_clone=1")
		} else if err := enableUnprivUserns(); err != nil {
			fmt.Printf("  ✗ could not enable unprivileged userns: %v\n", err)
		} else {
			fmt.Println("  ✓ unprivileged user namespaces enabled")
			ok = true
		}
	}

	// 5b. network capabilities on the sklet binary — the agent's Network
	//     Controller binds DNS on :53 (cap_net_bind_service) and joining the
	//     WireGuard mesh creates wg0 (cap_net_admin), both as a plain user.
	//     File capabilities live in an xattr that chown and reinstall both
	//     clear silently (discovered live: a chown -R on ~/.local wiped them,
	//     and the only symptom was "create wg0: operation not permitted"),
	//     so this step re-applies them idempotently on every run.
	if selfPath, perr := os.Executable(); perr != nil {
		fmt.Printf("  ! sklet caps: cannot resolve own binary path: %v\n", perr)
	} else if !hasBin("setcap") {
		ok = false
		if root {
			if err := installLibcap(); err != nil {
				fmt.Printf("  ✗ sklet caps: setcap unavailable and install failed: %v\n", err)
			} else if err := applySkletCaps(selfPath); err != nil {
				fmt.Printf("  ✗ sklet caps: %v\n", err)
			} else {
				fmt.Printf("  ✓ network capabilities set on %s\n", selfPath)
				ok = true
			}
		} else {
			fmt.Println("  ✗ setcap MISSING (package libcap2-bin)")
			todo = append(todo, libcapInstallCmd(), fmt.Sprintf("sudo setcap '%s' %s", skletCaps, selfPath))
		}
	} else if skletCapsPresent(selfPath) {
		fmt.Printf("  ✓ network capabilities on %s (dns :53 + wg0)\n", selfPath)
	} else {
		ok = false
		if root {
			if err := applySkletCaps(selfPath); err != nil {
				fmt.Printf("  ✗ sklet caps: %v\n", err)
			} else {
				fmt.Printf("  ✓ network capabilities set on %s\n", selfPath)
				ok = true
			}
		} else {
			fmt.Printf("  ✗ network capabilities MISSING on %s (dns :53 + wg0 need them)\n", selfPath)
			todo = append(todo, fmt.Sprintf("sudo setcap '%s' %s", skletCaps, selfPath))
		}
	}

	// 5c. the launcher: a capability-carrying copy of sklet that starts the
	//     agent, so a later `sklet upgrade` swaps the binary without sudo.
	if selfPath, perr := os.Executable(); perr == nil {
		if u, lerr := user.Lookup(hu.name); lerr != nil {
			fmt.Printf("  ! launcher: %v\n", lerr)
		} else if LauncherReady(u.HomeDir) && !root {
			fmt.Printf("  ✓ launcher %s (upgrades need no sudo)\n", LauncherPath(u.HomeDir))
		} else if root {
			if err := installLauncher(selfPath, hu); err != nil {
				ok = false
				fmt.Printf("  ✗ launcher: %v\n", err)
			} else {
				fmt.Printf("  ✓ launcher installed at %s — run `sklet host configure` as %s to start the agent through it\n", LauncherPath(u.HomeDir), hu.name)
			}
		} else {
			ok = false
			fmt.Printf("  ✗ launcher MISSING at %s (every upgrade would need sudo)\n", LauncherPath(u.HomeDir))
			todo = append(todo, "sudo "+selfPath+" host setup")
		}
	}

	// 6. data disk — where deployed spaces keep their named volumes. Reported
	//    even in check mode: a node with none refuses to run a space with
	//    volumes rather than dropping a database into the runtime cache.
	if dir := xdg.DataRoot(); dir != "" {
		if err := ensureDataRoot(dir, hu, root); err != nil {
			fmt.Printf("  ✗ data root %s: %v\n", dir, err)
			ok = false
		} else {
			fmt.Printf("  ✓ data root %s (volumes stored here, outside the cache)\n", dir)
		}
	} else {
		fmt.Printf("  ! no data root: %s is unset, so this node cannot store a space's volumes durably\n", xdg.DataRootEnv)
		fmt.Printf("    a space declaring named volumes will be refused; point %s at the host data disk (e.g. /data/sklp)\n", xdg.DataRootEnv)
	}

	// 5. cgroup v2 + controller delegation. A cpu/memory cap on a space is
	//    applied rootless through the user slice, which must delegate the
	//    controller; systemd < 252 delegates only pids+memory by default, so
	//    a CPU-capped CI run is refused there until the drop-in exists.
	if !cgroupV2() {
		fmt.Println("  ! cgroup v1 detected — resource limits may be partial (cgroup v2 recommended)")
	} else if missing := userSliceMissingControllers(hu); len(missing) == 0 {
		fmt.Println("  ✓ cgroup v2 with cpu/memory/pids delegated to the user slice")
	} else {
		ok = false
		fmt.Printf("  ✗ cgroup v2: the user slice does not delegate %s (space caps dropped, CI runs refused)\n", strings.Join(missing, "+"))
		if root {
			if err := installCgroupDelegation(); err != nil {
				fmt.Printf("  ✗ cgroup delegation: %v\n", err)
			} else {
				fmt.Printf("  ✓ wrote %s — re-login (or restart user@%d.service) for the slice to pick it up\n", cgroupDelegateDropIn, hu.uid)
				ok = true
			}
		} else {
			todo = append(todo, "sudo sklet host setup   # writes "+cgroupDelegateDropIn+" and reloads systemd; re-login after")
		}
	}

	fmt.Println()
	if ok {
		fmt.Println("Host is ready to run rootless spaces.")
		return 0
	}
	if root {
		// Applied what we could; a remaining failure was printed above.
		fmt.Println("Some prerequisites could not be applied — see the lines marked ✗ above.")
		return 1
	}
	fmt.Println("Missing prerequisites. Run:")
	fmt.Println()
	fmt.Println("  sudo sklet host setup")
	fmt.Println()
	fmt.Println("or apply them manually:")
	for _, c := range todo {
		fmt.Printf("  %s\n", c)
	}
	return 1
}

// --- checks (unprivileged) --------------------------------------------------

func hasUIDMap() bool {
	_, e1 := exec.LookPath("newuidmap")
	_, e2 := exec.LookPath("newgidmap")
	return e1 == nil && e2 == nil
}

// hostPreflight returns a non-empty, actionable message when a HARD rootless
// prerequisite is missing, so `sklp deploy` can fail early with a clear hint
// instead of the cryptic runc "nsexec: mapping tool not present". Empty = ready.
func HostPreflight() string {
	if !hasUIDMap() {
		return "rootless prerequisite missing: newuidmap/newgidmap (package uidmap).\n" +
			"  Run:  sudo sklet host setup   (or: " + uidmapInstallCmd() + ")"
	}
	return ""
}

// hasSubIDRange reports whether path grants user a range of at least 65536 ids.
func hasSubIDRange(path string, hu hostUser) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	uidToken := strconv.Itoa(hu.uid)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) != 3 {
			continue
		}
		if parts[0] != hu.name && parts[0] != uidToken {
			continue
		}
		if count, err := strconv.Atoi(parts[2]); err == nil && count >= 65536 {
			return true
		}
	}
	return false
}

func lingerEnabled(name string) bool {
	out, err := exec.Command("loginctl", "show-user", name, "--property=Linger").CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "Linger=yes")
}

// unprivUsernsClone returns the sysctl value, or "" when the knob does not exist
// (non-Debian kernels: unprivileged userns is on by default).
func unprivUsernsClone() string {
	b, err := os.ReadFile("/proc/sys/kernel/unprivileged_userns_clone")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

const cgroupDelegateDropIn = "/etc/systemd/system/user@.service.d/sklp-delegate.conf"

var cgroupDelegatedControllers = []string{"cpu", "memory", "pids"}

// userSliceMissingControllers lists the controllers the user's systemd slice
// does not delegate. Without a live session the slice has no cgroup to read,
// so the drop-in's presence stands in for it.
func userSliceMissingControllers(hu hostUser) []string {
	path := fmt.Sprintf("/sys/fs/cgroup/user.slice/user-%d.slice/user@%d.service/cgroup.controllers", hu.uid, hu.uid)
	data, err := os.ReadFile(path)
	if err != nil {
		if _, derr := os.Stat(cgroupDelegateDropIn); derr == nil {
			return nil
		}
		return cgroupDelegatedControllers
	}
	have := map[string]bool{}
	for _, c := range strings.Fields(string(data)) {
		have[c] = true
	}
	var missing []string
	for _, c := range cgroupDelegatedControllers {
		if !have[c] {
			missing = append(missing, c)
		}
	}
	return missing
}

func installCgroupDelegation() error {
	if err := os.MkdirAll(filepath.Dir(cgroupDelegateDropIn), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(cgroupDelegateDropIn, []byte("[Service]\nDelegate=cpu cpuset io memory pids\n"), 0o644); err != nil {
		return err
	}
	return run("systemctl", "daemon-reload")
}

func cgroupV2() bool {
	// cgroup2 mounts a magic-typed fs at /sys/fs/cgroup; the controllers file
	// only exists on the unified hierarchy.
	_, err := os.Stat("/sys/fs/cgroup/cgroup.controllers")
	return err == nil
}

// --- fixes (root only) ------------------------------------------------------

func installUIDMap() error {
	switch {
	case hasBin("apt-get"):
		if err := run("apt-get", "update", "-qq"); err != nil {
			return err
		}
		return runEnv([]string{"DEBIAN_FRONTEND=noninteractive", "NEEDRESTART_MODE=a"},
			"apt-get", "install", "-y", "uidmap")
	case hasBin("dnf"):
		return run("dnf", "install", "-y", "shadow-utils")
	case hasBin("apk"):
		return run("apk", "add", "--no-cache", "shadow-uidmap")
	default:
		return fmt.Errorf("no supported package manager (apt-get/dnf/apk) found")
	}
}

func uidmapInstallCmd() string {
	switch {
	case hasBin("apt-get"):
		return "sudo apt-get install -y uidmap"
	case hasBin("dnf"):
		return "sudo dnf install -y shadow-utils"
	case hasBin("apk"):
		return "sudo apk add shadow-uidmap"
	default:
		return "install the uidmap package (newuidmap/newgidmap) with your package manager"
	}
}

// subIDRangeSize is how many ids grantSubIDRange allocates — enough headroom
// for a rootless space's mapped uid/gid range (see deploynet_linux.go).
const subIDRangeSize = 65536

// grantSubIDRange allocates a subuid+subgid range for the user. The range is
// picked to start above every existing entry in EITHER file (not just the
// target user's own), so it can never collide with a range a different user
// already holds — e.g. Ubuntu 24.04's cloud image pre-allocates
// ubuntu:100000:65536, and blindly requesting the same 100000-165535 for a
// second user corrupts both entries' effective range (newuidmap then fails
// with an opaque "no child processes" at every rootless container start,
// discovered on node-d and reproduced on node-a).
func grantSubIDRange(hu hostUser) error {
	base := nextFreeSubIDBase(subIDRangeSize, "/etc/subuid", "/etc/subgid")
	rng := fmt.Sprintf("%d-%d", base, base+subIDRangeSize-1)
	add := []string{"--add-subuids", rng, "--add-subgids", rng, hu.name}
	return run("usermod", add...)
}

// nextFreeSubIDBase scans every subuid/subgid entry across paths (any user,
// not just the one being granted) and returns the first multiple of size at
// or above 100000 whose [base, base+size) range overlaps none of them. Never
// returns a base below 100000 — that low range is reserved for regular
// system/login uids, matching where subuid ranges conventionally start.
func nextFreeSubIDBase(size int, paths ...string) int {
	const start = 100000
	type span struct{ lo, hi int } // [lo, hi)
	var taken []span
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.Split(line, ":")
			if len(parts) != 3 {
				continue
			}
			lo, err1 := strconv.Atoi(parts[1])
			count, err2 := strconv.Atoi(parts[2])
			if err1 != nil || err2 != nil {
				continue
			}
			taken = append(taken, span{lo, lo + count})
		}
		f.Close()
	}

	for base := start; ; base += size {
		hi := base + size
		overlaps := false
		for _, t := range taken {
			if base < t.hi && hi > t.lo {
				overlaps = true
				break
			}
		}
		if !overlaps {
			return base
		}
	}
}

func enableLinger(name string) error { return run("loginctl", "enable-linger", name) }

// skletCaps is the exact file-capability set the agent needs: DNS on :53
// (bind_service) and wg0 creation for the mesh (net_admin).
const skletCaps = "cap_net_bind_service,cap_net_admin=+ep"

// skletCapsPresent verifies the binary carries exactly skletCaps, via
// `setcap -v` (verify-only, works unprivileged).
func skletCapsPresent(path string) bool {
	return exec.Command("setcap", "-v", skletCaps, path).Run() == nil
}

func applySkletCaps(path string) error { return run("setcap", skletCaps, path) }

func installLibcap() error {
	switch {
	case hasBin("apt-get"):
		return runEnv([]string{"DEBIAN_FRONTEND=noninteractive", "NEEDRESTART_MODE=a"},
			"apt-get", "install", "-y", "libcap2-bin")
	case hasBin("dnf"):
		return run("dnf", "install", "-y", "libcap")
	case hasBin("apk"):
		return run("apk", "add", "--no-cache", "libcap")
	default:
		return fmt.Errorf("no supported package manager (apt-get/dnf/apk) found")
	}
}

func libcapInstallCmd() string {
	switch {
	case hasBin("apt-get"):
		return "sudo apt-get install -y libcap2-bin"
	case hasBin("dnf"):
		return "sudo dnf install -y libcap"
	case hasBin("apk"):
		return "sudo apk add libcap"
	default:
		return "install the libcap tools (setcap) with your package manager"
	}
}

func enableUnprivUserns() error {
	return run("sysctl", "-w", "kernel.unprivileged_userns_clone=1")
}

// --- exec helpers -----------------------------------------------------------

func hasBin(name string) bool { _, err := exec.LookPath(name); return err == nil }

func run(name string, args ...string) error { return runEnv(nil, name, args...) }

func runEnv(extraEnv []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), extraEnv...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ensureDataRoot makes the node's data root usable by the space user: it must
// exist and be owned by them, since a rootless supervisor creates each space's
// volume directories underneath it.
//
// Under root it creates and chowns; unprivileged it only reports, like every
// other check here. It deliberately does not mount anything: which disk backs
// the path is the provisioning layer's decision (cloud-init, fstab), not this
// command's.
func ensureDataRoot(dir string, hu hostUser, root bool) error {
	fi, err := os.Stat(dir)
	switch {
	case err == nil && !fi.IsDir():
		return fmt.Errorf("exists but is not a directory")
	case err != nil && !os.IsNotExist(err):
		return err
	case err != nil:
		if !root {
			return fmt.Errorf("does not exist (run under sudo to create it)")
		}
		if mkErr := os.MkdirAll(dir, 0o700); mkErr != nil {
			return mkErr
		}
	}
	if ownedBy(dir, hu.uid) {
		return nil
	}
	if !root {
		return fmt.Errorf("not owned by %q (run under sudo to fix)", hu.name)
	}
	return os.Chown(dir, hu.uid, hu.uid)
}

// ownedBy reports whether path's owning uid is uid. A stat failure answers
// false, so the caller reports a problem rather than assuming ownership.
func ownedBy(path string, uid int) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == uid
}
