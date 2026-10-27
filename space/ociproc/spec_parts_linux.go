//go:build linux

package ociproc

import (
	"bufio"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"
)

// spec_parts_linux.go holds the SECURITY-SENSITIVE pieces of the OCI runtime
// spec that every runc-spec producer (the run/space path in runtime/spec.go and
// the build path in runnerbuild) must agree on byte-for-byte. They live here, in
// the leaf package, for the same reason the IMDS blackhole does: a divergent
// copy of the rootless id-mapping or the capability set is a silent isolation /
// privilege drift, exactly the kind of bug a shared single source of truth
// prevents.

var (
	subuidPath = "/etc/subuid"
	subgidPath = "/etc/subgid"
)

// ResolveIDMappings is ResolveIdentity rendered as OCI mappings.
func ResolveIDMappings(hostUID, hostGID int) (uid, gid []map[string]any, err error) {
	id, err := ResolveIdentity(hostUID, hostGID)
	if err != nil {
		return nil, nil, err
	}
	uid, gid = id.Mappings()
	return uid, gid, nil
}

// IDMappings is the deterministic test helper for the common rootless mapping
// shape. Production callers use ResolveIDMappings so the delegated subuid/subgid
// range comes from the host instead of a hardcoded assumption.
func IDMappings(hostUID, hostGID int) (uid, gid []map[string]any) {
	return IDMappingsWithSubIDs(hostUID, hostGID, 100000, 65536, 100000, 65536)
}

func IDMappingsWithSubIDs(hostUID, hostGID, uidBase, uidCount, gidBase, gidCount int) (uid, gid []map[string]any) {
	uid = []map[string]any{
		{"containerID": 0, "hostID": hostUID, "size": 1},
		{"containerID": 1, "hostID": uidBase, "size": uidCount},
	}
	gid = []map[string]any{
		{"containerID": 0, "hostID": hostGID, "size": 1},
		{"containerID": 1, "hostID": gidBase, "size": gidCount},
	}
	return uid, gid
}

func currentSubIDName(hostUID int) (string, error) {
	u, err := user.Current()
	if err == nil && u.Username != "" {
		return u.Username, nil
	}
	return strconv.Itoa(hostUID), nil
}

func readSubIDRange(path, name string, hostUID int) (base, count int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, fmt.Errorf("read %s for rootless id mapping: %w", path, err)
	}
	defer f.Close()

	uidToken := strconv.Itoa(hostUID)
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
		if parts[0] != name && parts[0] != uidToken {
			continue
		}
		base, err := strconv.Atoi(parts[1])
		if err != nil {
			return 0, 0, fmt.Errorf("%s: invalid subid base in %q", path, line)
		}
		count, err := strconv.Atoi(parts[2])
		if err != nil {
			return 0, 0, fmt.Errorf("%s: invalid subid count in %q", path, line)
		}
		if count < 65536 {
			return 0, 0, fmt.Errorf("%s: subid range for %q is too small: %d < 65536", path, parts[0], count)
		}
		return base, count, nil
	}
	if err := sc.Err(); err != nil {
		return 0, 0, fmt.Errorf("scan %s: %w", path, err)
	}
	return 0, 0, fmt.Errorf("%s: no subid range for %q or uid %d", path, name, hostUID)
}

// secondSubIDRange is the first delegated range of name other than the one
// at mainBase, provided it does not overlap it; count is 0 when there is none.
func secondSubIDRange(path, name string, hostUID, mainBase, mainCount int) (base, count int) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0
	}
	uidToken := strconv.Itoa(hostUID)
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.Split(strings.TrimSpace(line), ":")
		if len(parts) != 3 || (parts[0] != name && parts[0] != uidToken) {
			continue
		}
		b, err1 := strconv.Atoi(parts[1])
		c, err2 := strconv.Atoi(parts[2])
		if err1 != nil || err2 != nil || c < 1 || b == mainBase {
			continue
		}
		if b < mainBase+mainCount && mainBase < b+c {
			continue
		}
		return b, c
	}
	return 0, 0
}

// ResolveSubIDBase returns the host subuid/subgid range actually granted to
// hostUID in /etc/subuid + /etc/subgid. It is the single source of truth the
// space network (spacenet) shares with the container id-mapping above, so a
// host whose user got a non-default base (e.g. the 2nd cloud user lands at
// 165536, not 100000) maps the same range both ways instead of failing
// newuidmap with "uid range not allowed".
func ResolveSubIDBase(hostUID int) (uidBase, gidBase, count int, err error) {
	name, err := currentSubIDName(hostUID)
	if err != nil {
		return 0, 0, 0, err
	}
	ub, uc, err := readSubIDRange(subuidPath, name, hostUID)
	if err != nil {
		return 0, 0, 0, err
	}
	gb, gc, err := readSubIDRange(subgidPath, name, hostUID)
	if err != nil {
		return 0, 0, 0, err
	}
	if uc < gc {
		gc = uc
	}
	return ub, gb, gc, nil
}

// DefaultCapabilities is the baseline capability set shared by every sklp
// container — sized to cover go build, pnpm install, git, and chown for tooling
// caches. CAP_SYS_ADMIN is INTENTIONALLY absent: the embedded runtime must never
// grant it, since the user-namespace remap is the only thing protecting the host
// from a misbehaving step. Callers that need an extra capability append it
// EXPLICITLY to a copy (see the build path's CAP_NET_ADMIN for the in-netns
// blackhole hook), so any privilege beyond this baseline is visible at its call
// site rather than buried in a divergent constant.
func DefaultCapabilities() []string {
	return []string{
		"CAP_AUDIT_WRITE", "CAP_KILL", "CAP_NET_BIND_SERVICE",
		"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FOWNER", "CAP_FSETID",
		"CAP_SETGID", "CAP_SETUID", "CAP_SETPCAP", "CAP_SETFCAP",
		"CAP_NET_RAW", "CAP_SYS_CHROOT", "CAP_MKNOD",
	}
}

// DefaultMaskedPaths returns the kernel pseudo-files every sklp container masks
// (bind-mounted to /dev/null so a read returns nothing). These leak host state
// or let a process poke the host kernel: /proc/kcore is raw physical memory,
// /proc/sysrq-trigger reboots the box, the rest expose host hardware/timer/irq
// detail. The user-namespace remap already denies most writes, but masking is a
// free second layer and the runc/Docker baseline — its absence was the gap.
func DefaultMaskedPaths() []string {
	return []string{
		"/proc/asound", "/proc/acpi", "/proc/kcore", "/proc/keys",
		"/proc/latency_stats", "/proc/timer_list", "/proc/timer_stats",
		"/proc/sched_debug", "/proc/scsi", "/proc/sysrq-trigger",
		"/sys/firmware", "/sys/devices/virtual/powercap",
	}
}

// DefaultReadonlyPaths returns the /proc bus/fs/irq/sys trees mounted read-only
// in every sklp container: a process may read them but never write host kernel
// knobs through them. Mirrors the runc/Docker default set.
// NestableMaskedPaths drops the /proc entries: any mount under /proc (masks and
// read-only binds alike) is a hidden entry the kernel protects by refusing a
// nested proc mount with EPERM, so a runc inside the container could never
// start. Keeps the /sys masks. Callers must drop the /proc read-only paths too.
func NestableMaskedPaths() []string {
	var out []string
	for _, p := range DefaultMaskedPaths() {
		if !strings.HasPrefix(p, "/proc/") {
			out = append(out, p)
		}
	}
	return out
}

// NestableSeccomp is DefaultSeccomp plus the syscalls runc itself needs to
// create a container inside this one: pivot_root to jail the step, and the new
// mount API runc uses for idmapped and bind mounts. Only the CI runner
// (Service.NestedRuntime) gets it; an application service never needs to nest.
func NestableSeccomp() map[string]any {
	prof := DefaultSeccomp()
	prof["syscalls"] = append(prof["syscalls"].([]map[string]any), map[string]any{
		"names": []string{"pivot_root", "open_tree", "move_mount", "fsopen", "fsconfig",
			"fsmount", "fspick", "mount_setattr", "sethostname", "setdomainname"},
		"action": "SCMP_ACT_ALLOW",
	})
	return prof
}

func DefaultReadonlyPaths() []string {
	return []string{
		"/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger",
	}
}

// DefaultSeccomp returns the OCI linux.seccomp object shared by every sklp
// container: a syscall ALLOWLIST whose default action is SCMP_ACT_ERRNO (EPERM),
// not KILL — a denied syscall fails the call rather than killing the step, which
// keeps a build that probes an unavailable syscall (common in apt/dpkg) limping
// instead of crashing. The list is the runc/Docker default profile DELIBERATELY
// kept userns-friendly: clone/unshare/mount/keyctl stay allowed because sklp
// containers run inside a user namespace where those are already constrained,
// and the build path's dpkg/apt + slirp tap setup genuinely need them. The
// dangerous-but-not-here syscalls (kexec, init_module, bpf, ptrace of others,
// reboot, swapon, mount of host fs without userns) are denied by omission.
//
// It is the ONE definition of the profile — run/service/build paths all embed
// it, so a divergent copy can never silently widen one path's syscall surface.
func DefaultSeccomp() map[string]any {
	arches := []string{"SCMP_ARCH_X86_64", "SCMP_ARCH_X86", "SCMP_ARCH_X32",
		"SCMP_ARCH_AARCH64", "SCMP_ARCH_ARM"}
	allow := func(names ...string) map[string]any {
		return map[string]any{"names": names, "action": "SCMP_ACT_ALLOW"}
	}
	return map[string]any{
		"defaultAction": "SCMP_ACT_ERRNO",
		"architectures": arches,
		"syscalls": []map[string]any{
			allow(seccompAllowList()...),
		},
	}
}

// seccompAllowList is the flat allowlist embedded by DefaultSeccomp. It is the
// runc/Docker default-allowed set, pruned to what sklp workloads (go build,
// pnpm, git, apt/dpkg, slirp tap, postgres/nats services) actually issue, and
// kept userns-compatible (clone/unshare/mount/keyctl present). Anything not
// here returns EPERM.
func seccompAllowList() []string {
	return []string{
		"accept", "accept4", "access", "adjtimex", "alarm", "bind", "brk",
		"capget", "capset", "chdir", "chmod", "chown", "chown32", "chroot",
		"clock_getres", "clock_gettime", "clock_nanosleep", "close",
		"close_range", "connect", "copy_file_range", "creat", "dup", "dup2",
		"dup3", "epoll_create", "epoll_create1", "epoll_ctl", "epoll_pwait",
		"epoll_wait", "eventfd", "eventfd2", "execve", "execveat", "exit",
		"exit_group", "faccessat", "faccessat2", "fadvise64", "fallocate",
		"fchdir", "fchmod", "fchmodat", "fchown", "fchown32", "fchownat",
		"fcntl", "fcntl64", "fdatasync", "fgetxattr", "flistxattr", "flock",
		"fork", "fremovexattr", "fsetxattr", "fstat", "fstat64", "fstatat64",
		"fstatfs", "fsync", "ftruncate", "ftruncate64", "futex", "futex_time64",
		"getcpu", "getcwd", "getdents", "getdents64", "getegid", "getegid32",
		"geteuid", "geteuid32", "getgid", "getgid32", "getgroups",
		"getgroups32", "getitimer", "getpeername", "getpgid", "getpgrp",
		"getpid", "getppid", "getpriority", "getrandom", "getresgid",
		"getresgid32", "getresuid", "getresuid32", "getrlimit", "getrusage",
		"getsid", "getsockname", "getsockopt", "gettid", "gettimeofday",
		"getuid", "getuid32", "getxattr", "inotify_add_watch",
		"inotify_init", "inotify_init1", "inotify_rm_watch", "io_destroy",
		"io_getevents", "io_setup", "io_submit", "ioctl", "ioprio_get",
		"ioprio_set", "ipc", "keyctl", "kill", "lchown", "lchown32", "lgetxattr",
		"link", "linkat", "listen", "listxattr", "llistxattr", "lremovexattr",
		"lseek", "lsetxattr", "lstat", "lstat64", "madvise", "memfd_create",
		"mincore", "mkdir", "mkdirat", "mknod", "mknodat", "mlock", "mlock2",
		"mlockall", "mmap", "mmap2", "mount", "mprotect", "mq_getsetattr",
		"mq_notify", "mq_open", "mq_timedreceive", "mq_timedsend", "mq_unlink",
		"mremap", "msgctl", "msgget", "msgrcv", "msgsnd", "msync", "munlock",
		"munlockall", "munmap", "nanosleep", "newfstatat", "open", "openat",
		"openat2", "pause", "pidfd_open", "pidfd_send_signal", "pipe", "pipe2",
		"poll", "ppoll", "prctl", "pread64", "preadv", "preadv2", "prlimit64",
		"pselect6", "pwrite64", "pwritev", "pwritev2", "read", "readahead",
		"readlink", "readlinkat", "readv", "recv", "recvfrom", "recvmmsg",
		"recvmsg", "remove", "removexattr", "rename", "renameat", "renameat2",
		"restart_syscall", "rmdir", "rseq", "rt_sigaction", "rt_sigpending",
		"rt_sigprocmask", "rt_sigqueueinfo", "rt_sigreturn", "rt_sigsuspend",
		"rt_sigtimedwait", "rt_tgsigqueueinfo", "sched_getaffinity",
		"sched_getattr", "sched_getparam", "sched_get_priority_max",
		"sched_get_priority_min", "sched_getscheduler", "sched_rr_get_interval",
		"sched_setaffinity", "sched_setattr", "sched_setparam",
		"sched_setscheduler", "sched_yield", "seccomp", "select", "semctl",
		"semget", "semop", "semtimedop", "send", "sendfile", "sendfile64",
		"sendmmsg", "sendmsg", "sendto", "setfsgid", "setfsgid32", "setfsuid",
		"setfsuid32", "setgid", "setgid32", "setgroups", "setgroups32",
		"setitimer", "setns", "setpgid", "setpriority", "setregid",
		"setregid32", "setresgid", "setresgid32", "setresuid", "setresuid32",
		"setreuid", "setreuid32", "setrlimit", "setsid", "setsockopt",
		"setuid", "setuid32", "setxattr", "shmat", "shmctl", "shmdt",
		"shmget", "shutdown", "sigaltstack", "signalfd", "signalfd4",
		"socket", "socketcall", "socketpair", "splice", "stat", "stat64",
		"statfs", "statfs64", "statx", "symlink", "symlinkat", "sync",
		"sync_file_range", "syncfs", "sysinfo", "tee", "tgkill", "time",
		"timer_create", "timer_delete", "timer_getoverrun", "timer_gettime",
		"timer_settime", "timerfd_create", "timerfd_gettime",
		"timerfd_settime", "times", "tkill", "truncate", "truncate64",
		"umask", "umount2", "uname", "unlink", "unlinkat", "unshare",
		"utime", "utimensat", "utimes", "vfork", "wait4", "waitid", "waitpid",
		"write", "writev", "clone", "clone3", "arch_prctl", "modify_ldt",
		"membarrier", "set_robust_list", "get_robust_list", "set_tid_address",
		"set_thread_area", "get_thread_area", "personality", "sysfs", "ustat",
		"fanotify_mark", "name_to_handle_at", "open_by_handle_at",
	}
}

const (
	uidMapPath = "/proc/self/uid_map"
	gidMapPath = "/proc/self/gid_map"
)

// mappedRangeAboveRoot returns how many ids starting at 1 the current user
// namespace maps contiguously, or 0 when it maps only root or is the initial
// namespace. A nested container may identity-map exactly that range: the
// kernel accepts a child map whose ids are all mapped in the parent.
func mappedRangeAboveRoot(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return rangeAboveRootFromMap(string(data))
}

func rangeAboveRootFromMap(m string) int {
	for _, line := range strings.Split(strings.TrimSpace(m), "\n") {
		var inner, outer, size int
		if _, err := fmt.Sscan(line, &inner, &outer, &size); err != nil {
			continue
		}
		if inner == 0 && size == 4294967295 {
			return 0 // the initial namespace: real root maps real users, leave it alone
		}
		if inner <= 1 && inner+size > 1 {
			return inner + size - 1
		}
	}
	return 0
}

// ServiceCapabilities is the baseline for a DEPLOYED SERVICE, deliberately
// narrower than DefaultCapabilities: that set is sized for the CI runner (go
// build, pnpm install, git, chown of tooling caches), and a long-lived service
// inherited it only because the two shared one constant.
//
// What is kept is what an image's own entrypoint needs before it drops
// privileges — postgres chowns its data dir and setuids to `postgres`, nginx
// and nats do the same shape of thing:
//
//	CHOWN, DAC_OVERRIDE, FOWNER, FSETID   own the data dir it was handed
//	SETGID, SETUID                        become its unprivileged user
//	KILL                                  signal its own children
//
// What is dropped matters more on a node hosting several apps:
//
//	NET_RAW      forges packets and ARP-spoofs INSIDE the space bridge, so one
//	             service can impersonate another of the same space — the very
//	             traffic that carries the database password
//	MKNOD        creates device nodes
//	SYS_CHROOT   escapes into another root
//	SETPCAP      edits its own capability set
//	SETFCAP      sets file capabilities that survive into a later exec
//	AUDIT_WRITE  writes the host audit log
//
// A service needing one of these asks for it in its manifest, so the privilege
// is visible where the service is declared rather than granted to everything.
func ServiceCapabilities() []string {
	return []string{
		"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FOWNER", "CAP_FSETID",
		"CAP_SETGID", "CAP_SETUID", "CAP_KILL",
	}
}

// NormalizeCapability accepts a capability with or without the CAP_ prefix and
// returns the OCI spelling, so a manifest may write either form. It returns ""
// for anything that is not a known capability name.
func NormalizeCapability(name string) string {
	n := strings.ToUpper(strings.TrimSpace(name))
	if n == "" {
		return ""
	}
	if !strings.HasPrefix(n, "CAP_") {
		n = "CAP_" + n
	}
	for _, known := range knownCapabilities {
		if n == known {
			return n
		}
	}
	return ""
}

// ManifestGrantableCapability is NormalizeCapability restricted to what a
// manifest may ask for. Known is not grantable: inside the userns container
// root is the space user, so DAC_READ_SEARCH alone reads every file that user
// owns (the node token included), SYS_PTRACE reads sibling memory, NET_ADMIN
// rewrites the space's routing. A service that truly needs one of these is a
// node operator's decision, not a deploy's.
func ManifestGrantableCapability(name string) string {
	n := NormalizeCapability(name)
	for _, refused := range manifestRefusedCapabilities {
		if n == refused {
			return ""
		}
	}
	return n
}

var manifestRefusedCapabilities = []string{
	"CAP_DAC_READ_SEARCH", "CAP_SYS_PTRACE", "CAP_NET_ADMIN", "CAP_MKNOD",
	"CAP_SYS_CHROOT", "CAP_SETPCAP", "CAP_SETFCAP",
}

// knownCapabilities is the set NormalizeCapability validates against: the Linux
// capabilities a rootless container could plausibly be granted. CAP_SYS_ADMIN is
// absent on purpose — the user-namespace remap is the only thing standing
// between a service and the host, and nothing in a manifest may hand it over.
var knownCapabilities = []string{
	"CAP_AUDIT_WRITE", "CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_DAC_READ_SEARCH",
	"CAP_FOWNER", "CAP_FSETID", "CAP_IPC_LOCK", "CAP_KILL", "CAP_MKNOD",
	"CAP_NET_ADMIN", "CAP_NET_BIND_SERVICE", "CAP_NET_BROADCAST", "CAP_NET_RAW",
	"CAP_SETFCAP", "CAP_SETGID", "CAP_SETPCAP", "CAP_SETUID", "CAP_SYS_CHROOT",
	"CAP_SYS_NICE", "CAP_SYS_PTRACE", "CAP_SYS_RESOURCE",
}
