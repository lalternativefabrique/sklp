//go:build linux

// Command sklp-vzagent is the guest side of the vz backend: PID 1 in the
// Virtualization.framework VM. It mounts what the runtime needs, brings the
// NAT interface up, then serves one command per vsock connection, streaming
// its output back framed and reporting the exit status.
package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	agentPort = 1024
	shareTag  = "hostshare"
	cacheTag  = "sktcache"
)

type request struct {
	Cwd  string   `json:"cwd"`
	Env  []string `json:"env"`
	Argv []string `json:"argv"`
	TTY  bool     `json:"tty"`
	Cols int      `json:"cols"`
	Rows int      `json:"rows"`
}

type reply struct {
	Exit int    `json:"exit"`
	Err  string `json:"err,omitempty"`
}

// hostHome reads the mount point the host asked for from the kernel command
// line: as PID 1 the agent has no environment to read it from.
func hostHome() string {
	b, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return "/root"
	}
	for _, f := range strings.Fields(string(b)) {
		if v, ok := strings.CutPrefix(f, "sklp.home="); ok {
			return v
		}
	}
	return "/root"
}

// setBootClock sets the wall clock from the sklp.now=<unix-seconds> kernel
// argument the host stamps at VM start. The guest has no RTC and no NTP: it
// boots at whatever time the kernel defaults to and freezes across host
// sleeps, and a clock hours behind makes apt reject repository signatures as
// "not yet valid". The host re-syncs periodically over a session; this only
// covers the window before the first one.
func setBootClock() {
	b, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return
	}
	for _, f := range strings.Fields(string(b)) {
		v, ok := strings.CutPrefix(f, "sklp.now=")
		if !ok {
			continue
		}
		sec, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return
		}
		tv := syscall.Timeval{Sec: sec}
		if err := syscall.Settimeofday(&tv); err != nil {
			logf("clock: settimeofday: %v", err)
		} else {
			logf("clock: set from host (%d)", sec)
		}
		return
	}
}

// agentLogPath is where the agent keeps its own record. Its stderr goes to the
// serial console, which only the host sees and only with SKLP_VZ_DEBUG — so
// what the VM did to itself (mounts, dhcp, vsock) was unreadable from inside
// it, which is where anyone debugging a space already is.
const agentLogPath = "/run/sklp-vzagent.log"

var agentLog *os.File

func logf(format string, a ...any) {
	line := fmt.Sprintf("[vzagent] "+format+"\n", a...)
	fmt.Fprint(os.Stderr, line)
	if agentLog != nil {
		fmt.Fprintf(agentLog, "%s %s", time.Now().UTC().Format(time.RFC3339), line)
	}
}

// openAgentLog starts the on-disk record. Called after /run is mounted; a
// failure is reported to stderr and leaves logging console-only rather than
// stopping the boot.
func openAgentLog() {
	f, err := os.OpenFile(agentLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[vzagent] cannot open %s: %v\n", agentLogPath, err)
		return
	}
	agentLog = f
}

// registerBinfmt enables the qemu-user handlers update-binfmts recorded when
// the guest image was built, so a build of a foreign platform can run its RUN
// steps. Best-effort: a guest without the package only loses cross-platform
// builds.
func registerBinfmt() {
	const dir = "/proc/sys/fs/binfmt_misc"
	os.MkdirAll(dir, 0o755)
	if err := syscall.Mount("binfmt_misc", dir, "binfmt_misc", 0, ""); err != nil && err != syscall.EBUSY {
		logf("mount %s: %v", dir, err)
		return
	}
	if _, err := os.Stat("/usr/sbin/update-binfmts"); err != nil {
		return
	}
	if out, err := exec.Command("/usr/sbin/update-binfmts", "--enable").CombinedOutput(); err != nil {
		logf("update-binfmts --enable: %v: %s", err, strings.TrimSpace(string(out)))
		return
	}
	logf("binfmt handlers registered")
}

func mountAll(home string) {
	type m struct{ src, dst, fs string }
	for _, e := range []m{
		{"proc", "/proc", "proc"},
		{"sysfs", "/sys", "sysfs"},
		{"devtmpfs", "/dev", "devtmpfs"},
		{"tmpfs", "/run", "tmpfs"},
		{"cgroup2", "/sys/fs/cgroup", "cgroup2"},
	} {
		os.MkdirAll(e.dst, 0o755)
		if err := syscall.Mount(e.src, e.dst, e.fs, 0, ""); err != nil && err != syscall.EBUSY {
			logf("mount %s: %v", e.dst, err)
		}
	}
	os.MkdirAll("/dev/pts", 0o755)
	syscall.Mount("devpts", "/dev/pts", "devpts", 0, "")
	os.MkdirAll("/dev/shm", 0o1777)
	syscall.Mount("tmpfs", "/dev/shm", "tmpfs", 0, "")

	if err := os.MkdirAll(home, 0o755); err != nil {
		logf("mkdir %s: %v", home, err)
	}
	if err := syscall.Mount(shareTag, home, "virtiofs", 0, ""); err != nil {
		logf("virtiofs %s (tag %s): %v", home, shareTag, err)
	} else if ents, rerr := os.ReadDir(home); rerr != nil {
		logf("virtiofs %s mounted but unreadable: %v", home, rerr)
	} else {
		logf("virtiofs %s mounted, %d entries", home, len(ents))
	}
	cache := home + "/.cache/skt"
	os.MkdirAll(cache, 0o755)
	if err := syscall.Mount(cacheTag, cache, "virtiofs", 0, ""); err != nil {
		logf("virtiofs %s: %v", cache, err)
	}
}

// netUp brings the NAT interface up with DHCP. The VZ gateway hands out an
// address but no usable resolver, so DNS is written statically.
//
// The interface is discovered rather than guessed: its name depends on the
// kernel's naming scheme and on how many virtio devices the config declares,
// so a hardcoded list silently leaves the guest with no route the day it
// changes — which reads downstream as an unreachable registry, far from the
// cause.
func netUp() {
	guestCommand("ip", "link", "set", "lo", "up").Run()

	ifn, err := firstEthernet()
	if err != nil {
		logf("NET: no ethernet interface found: %v (the guest will have no egress)", err)
		return
	}
	if out, err := guestCommand("ip", "link", "set", ifn, "up").CombinedOutput(); err != nil {
		logf("NET: link up %s: %v: %s", ifn, err, out)
	}
	if out, err := guestCommand("udhcpc", "-i", ifn, "-q", "-n", "-t", "5").CombinedOutput(); err != nil {
		logf("NET: dhcp on %s failed: %v: %s", ifn, err, out)
	} else {
		logf("NET: %s configured by dhcp", ifn)
	}
	// Report the address the guest ended up with: it is what the cockpit's VM
	// panel reads back, so a mismatch between the two points at the reader
	// rather than at dhcp.
	if addrs := ifaceAddrs(ifn); len(addrs) > 0 {
		logf("NET: %s has %s", ifn, strings.Join(addrs, " "))
	} else {
		logf("NET: %s has no address after dhcp", ifn)
	}
	os.WriteFile("/etc/resolv.conf", []byte("nameserver 1.1.1.1\nnameserver 8.8.8.8\n"), 0o644)
}

// guestCommand builds an exec.Cmd whose binary is resolved against the guest
// PATH and which carries it in its environment. As PID 1 this agent inherits
// no environment at all, so a bare exec.Command cannot find `ip` or `udhcpc`
// even though the rootfs ships both.
func guestCommand(name string, args ...string) *exec.Cmd {
	bin, err := guestLookPath(name)
	if err != nil {
		bin = name
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = guestBaseEnv(false)
	return cmd
}

// ifaceAddrs lists an interface's IPv4 addresses over netlink.
func ifaceAddrs(name string) []string {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.To4() != nil {
			out = append(out, ipnet.String())
		}
	}
	return out
}

// firstEthernet returns the name of the first non-loopback interface the
// kernel created, which for this VM is the single virtio NIC.
func firstEthernet() (string, error) {
	ents, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return "", err
	}
	for _, e := range ents {
		name := e.Name()
		if name == "lo" {
			continue
		}
		return name, nil
	}
	return "", fmt.Errorf("only loopback present in /sys/class/net")
}

// framedWriter prefixes every line with its stream tag so the host can split
// stdout from stderr over the single vsock connection.
type framedWriter struct {
	mu  *sync.Mutex
	w   io.Writer
	tag byte
}

func (f framedWriter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, line := range strings.SplitAfter(string(p), "\n") {
		if line == "" {
			continue
		}
		fmt.Fprintf(f.w, "%c%s\n", f.tag, strings.TrimRight(line, "\n"))
	}
	return len(p), nil
}

// guestBaseEnv builds the environment a relayed command needs. PID 1 inherits
// none, so PATH and HOME are set explicitly before the host's forwarded
// variables are appended on top.
// guestCacheDir is where the runtime cache lives inside the VM. It must NOT
// be the one under the shared home: that directory reaches the guest over
// virtiofs, and overlayfs refuses to take a virtiofs upperdir — the mount
// fails, merged/ stays the read-only lowerdir, and runc dies creating
// /dev/shm with "read-only file system".
//
// Keeping it on the VM's own disk also means image layers and space state do
// not travel over the share on every read.
const guestCacheDir = "/var/lib/sklp/cache"

func guestBaseEnv(tty bool) []string {
	home := hostHome()
	env := []string{
		"XDG_CACHE_HOME=" + guestCacheDir,
		// Unix sockets cannot bind on the virtiofs-shared home; keep every
		// runtime socket (spacenet attach, control) on the guest's tmpfs.
		"XDG_RUNTIME_DIR=/run",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=" + home,
		"USER=root",
		"SHELL=/bin/bash",
		"TERM=" + envOr("TERM", "xterm-256color"),
	}
	if tty {
		// Programs that check for a terminal read this to decide whether to
		// render full-screen; the host only asks for a TTY when it has one.
		env = append(env, "SKLP_TTY=1")
	}
	return env
}

// guestLookPath finds an executable using the guest PATH rather than the
// agent's own (empty) environment.
func guestLookPath(name string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	for _, dir := range strings.Split("/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", ":") {
		candidate := dir + "/" + name
		if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s: not found in guest PATH", name)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func serve(conn *os.File) {
	defer conn.Close()

	var req request
	dec := json.NewDecoder(conn)
	if err := dec.Decode(&req); err != nil {
		logf("decode request: %v", err)
		return
	}
	if len(req.Argv) == 0 {
		logf("empty argv")
		return
	}

	var mu sync.Mutex
	// exec.Command resolves argv[0] against THIS process's PATH, which as PID 1
	// is empty — setting cmd.Env does not affect the lookup. Resolve it against
	// the guest PATH explicitly.
	bin, lerr := guestLookPath(req.Argv[0])
	if lerr != nil {
		writeReply(&mu, conn, reply{Exit: 127, Err: lerr.Error()})
		return
	}
	cmd := exec.Command(bin, req.Argv[1:]...)
	cmd.Dir = req.Cwd
	cmd.Env = append(guestBaseEnv(req.TTY), req.Env...)

	rest := io.MultiReader(dec.Buffered(), conn)

	if req.TTY {
		serveTTY(conn, &mu, cmd, req, rest)
		return
	}
	servePipes(conn, &mu, cmd, rest)
}

// serveTTY runs the command on a pty so full-screen programs (the cockpit, a
// space shell) behave as they would over `limactl shell`: one interleaved
// stream, a real terminal, and window-size changes.
func serveTTY(conn *os.File, mu *sync.Mutex, cmd *exec.Cmd, req request, rest io.Reader) {
	ptmx, tty, err := openPTY()
	if err != nil {
		writeReply(mu, conn, reply{Exit: 1, Err: err.Error()})
		return
	}
	defer ptmx.Close()
	_ = setWinsize(ptmx, req.Cols, req.Rows)

	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}

	if err := cmd.Start(); err != nil {
		tty.Close()
		writeReply(mu, conn, reply{Exit: 127, Err: err.Error()})
		return
	}
	// The child owns the slave now; keeping it open here would stop the master
	// from ever reporting EOF.
	tty.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		// A pty master returns EIO rather than EOF when the last slave closes,
		// which is the normal end of a session, not a failure.
		buf := make([]byte, 32*1024)
		for {
			n, rerr := ptmx.Read(buf)
			if n > 0 {
				mu.Lock()
				fmt.Fprintf(conn, "P%s\n", base64.StdEncoding.EncodeToString(buf[:n]))
				mu.Unlock()
			}
			if rerr != nil {
				return
			}
		}
	}()

	go relayControl(rest, cmd, ptmx)
	code := waitCode(cmd)
	<-done
	writeReply(mu, conn, reply{Exit: code})
}

// servePipes runs the command without a terminal, keeping stdout and stderr
// distinct so a caller redirecting one of them still gets what it expects.
func servePipes(conn *os.File, mu *sync.Mutex, cmd *exec.Cmd, rest io.Reader) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		writeReply(mu, conn, reply{Exit: 1, Err: err.Error()})
		return
	}
	cmd.Stdout = framedWriter{mu: mu, w: conn, tag: '1'}
	cmd.Stderr = framedWriter{mu: mu, w: conn, tag: '2'}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		writeReply(mu, conn, reply{Exit: 127, Err: err.Error()})
		return
	}
	go relayControl(rest, cmd, stdin)
	writeReply(mu, conn, reply{Exit: waitCode(cmd)})
}

func waitCode(cmd *exec.Cmd) int {
	if err := cmd.Wait(); err != nil {
		var ee *exec.ExitError
		if asExitError(err, &ee) {
			return ee.ExitCode()
		}
		return 1
	}
	return 0
}

func asExitError(err error, out **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok {
		*out = ee
	}
	return ok
}

// relayControl consumes the host's control stream for a running command:
// stdin data, EOF, window resizes and signals. Everything is line-framed and
// binary payloads are base64 so a keystroke can never look like a frame
// boundary.
func relayControl(r io.Reader, cmd *exec.Cmd, in io.WriteCloser) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		switch line[0] {
		case 'I':
			b, err := base64.StdEncoding.DecodeString(line[1:])
			if err == nil {
				in.Write(b)
			}
		case 'E':
			in.Close()
		case 'W':
			var cols, rows int
			if _, err := fmt.Sscanf(line[1:], "%d %d", &cols, &rows); err == nil {
				if f, ok := in.(*os.File); ok {
					_ = setWinsize(f, cols, rows)
				}
			}
		case 'S':
			var n int
			if _, err := fmt.Sscanf(line[1:], "%d", &n); err == nil && n > 0 && cmd.Process != nil {
				// With a pty the child is a session leader in its own process
				// group, so signal the group: that is what reaches a shell's
				// children too.
				if cmd.SysProcAttr != nil && cmd.SysProcAttr.Setsid {
					_ = syscall.Kill(-cmd.Process.Pid, syscall.Signal(n))
				} else {
					_ = cmd.Process.Signal(syscall.Signal(n))
				}
			}
		}
	}
}

func writeReply(mu *sync.Mutex, w io.Writer, r reply) {
	mu.Lock()
	defer mu.Unlock()
	b, _ := json.Marshal(r)
	fmt.Fprintf(w, "X%s\n", b)
}

func main() {
	// /proc must exist before the kernel command line can be read, and the
	// command line is where the host puts the home mount point.
	os.MkdirAll("/proc", 0o755)
	if err := syscall.Mount("proc", "/proc", "proc", 0, ""); err != nil && err != syscall.EBUSY {
		logf("mount /proc: %v", err)
	}
	home := hostHome()
	mountAll(home)
	if err := os.MkdirAll(guestCacheDir, 0o755); err != nil {
		logf("cache dir %s: %v", guestCacheDir, err)
	}
	openAgentLog()
	registerBinfmt()
	logf("guest up: home=%s cache=%s", home, guestCacheDir)
	setBootClock()
	netUp()

	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		logf("vsock socket: %v", err)
		os.Exit(1)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: agentPort}); err != nil {
		logf("vsock bind: %v", err)
		os.Exit(1)
	}
	if err := unix.Listen(fd, 4); err != nil {
		logf("vsock listen: %v", err)
		os.Exit(1)
	}
	go serveForward()
	logf("listening on vsock:%d", agentPort)

	for {
		nfd, _, err := unix.Accept(fd)
		if err != nil {
			logf("accept: %v", err)
			continue
		}
		// One goroutine per session: the VM is shared and long-lived now, so a
		// `sklp dev` holding its connection open for hours must not stop a
		// cockpit from opening beside it. The VM is powered off by the host,
		// which owns its lifetime, not from in here after a single command.
		go serve(os.NewFile(uintptr(nfd), "vsock-conn"))
	}
}
