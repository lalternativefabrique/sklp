//go:build darwin && arm64 && cgo

package limarelay

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The VM outlives the command that started it. It has to: `sklp dev` runs
// services for as long as you keep them up, and a per-run VM would hold the
// rootfs image open for that whole time, locking out every other sklp — the
// cockpit in another terminal, a second dev stack, anything.
//
// The shape is the one sklet already uses for its supervisor: re-exec this
// same binary with a hidden verb under Setsid, guard the single instance with
// a flock rather than with the socket, and record the pid so a later command
// can tell "running" from "crashed" without guessing.

// daemonVerb is the hidden subcommand the detached process runs. Double
// underscore marks it internal, matching `sklet __supervise`.
const daemonVerb = "__macos-vm"

func vzRunDir() string { return vzStateDir() }

func vzLockPath() string { return filepath.Join(vzRunDir(), "vm.lock") }
func vzPidPath() string  { return filepath.Join(vzRunDir(), "vm.pid") }
func vzLogPath() string  { return filepath.Join(vzRunDir(), "vm.log") }

// acquireVMLock takes the single-writer guard. Held for the daemon's entire
// life; the flock rather than the vsock port is the guard, because it also
// covers the window before the VM is up.
func acquireVMLock() (release func(), err error) {
	if err := os.MkdirAll(vzRunDir(), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(vzLockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if err == unix.EWOULDBLOCK {
			return nil, errVMBusy
		}
		return nil, err
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
		_ = os.Remove(vzLockPath())
	}, nil
}

// errVMBusy means another process holds the VM lock — which is the normal
// case once a daemon is up, not a failure.
var errVMBusy = fmt.Errorf("the macOS VM is already running")

// writePid records the daemon's pid atomically, so a reader never sees a
// half-written file.
func writePid(pid int) error {
	tmp, err := os.CreateTemp(vzRunDir(), "vm.pid.*")
	if err != nil {
		return err
	}
	if _, err := tmp.WriteString(strconv.Itoa(pid)); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), vzPidPath())
}

// daemonPID reports the running daemon's pid, or 0 when none is. A pidfile
// naming a dead process is removed on the way out, so a crashed daemon does
// not keep looking alive.
func daemonPID() int {
	b, err := os.ReadFile(vzPidPath())
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		_ = os.Remove(vzPidPath())
		return 0
	}
	if err := syscall.Kill(pid, 0); err != nil && err != syscall.EPERM {
		_ = os.Remove(vzPidPath())
		return 0
	}
	return pid
}

// DaemonRunning reports whether a macOS VM daemon is up.
func DaemonRunning() bool { return daemonPID() != 0 }

// StopDaemon asks the running daemon to exit, taking the VM down with it.
// Reports whether there was one to stop.
func StopDaemon() (bool, error) {
	pid := daemonPID()
	if pid == 0 {
		return false, nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if err == syscall.ESRCH {
			_ = os.Remove(vzPidPath())
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// StartDaemon launches the VM daemon if one is not already up. Exported for
// `sklp macos up`, which pre-warms it rather than waiting for the first
// command to pay the boot.
func StartDaemon() error { return startDaemon() }

// startDaemon launches the detached VM host and waits for it to answer.
// Returns nil when a daemon is up — including when another process won the
// race and started it first.
func startDaemon() error {
	if DaemonRunning() {
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(vzRunDir(), 0o700); err != nil {
		return err
	}
	logFile, err := os.OpenFile(vzLogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()

	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer devNull.Close()

	cmd := exec.Command(self, daemonVerb)
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Setsid detaches it from this process group, so the daemon survives the
	// command that started it — no Wait, no Kill on return.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the macOS VM: %w", err)
	}

	if err := waitDaemonReady(); err != nil {
		return fmt.Errorf("%w (see %s)", err, vzLogPath())
	}
	return nil
}

// waitDaemonReady polls until the daemon reports itself up. Booting the guest
// takes a couple of seconds cold; the deadline is generous because failing
// here would send the caller to a VM manager it may not have.
func waitDaemonReady() error {
	deadline := time.Now().Add(vzDaemonReadyTimeout)
	for time.Now().Before(deadline) {
		if DaemonRunning() {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("the macOS VM did not come up within %s", vzDaemonReadyTimeout)
}

const vzDaemonReadyTimeout = 90 * time.Second

// signalNotifyStop arms the shutdown signals: SIGTERM from `sklp macos down`,
// SIGINT when the daemon is run in the foreground for debugging.
func signalNotifyStop(ch chan os.Signal) {
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
}

// RunDaemon is the body of the detached process: it boots the VM and then
// stays alive holding it, because a VZVirtualMachine dies with the process
// that created it. Returns when asked to stop.
//
// It is the single writer to the rootfs image, guarded by the flock it takes
// first; every other sklp invocation talks to the guest over vsock instead of
// booting its own VM.
func RunDaemon() error {
	release, err := acquireVMLock()
	if err != nil {
		if err == errVMBusy {
			// Another daemon won the race between the caller's check and here.
			// That is success from the caller's point of view: a VM is up.
			return nil
		}
		return err
	}
	defer release()

	p, err := newVZProvider()
	if err != nil {
		return err
	}
	if err := p.bootPersistent(); err != nil {
		return err
	}
	if err := writePid(os.Getpid()); err != nil {
		return err
	}
	defer os.Remove(vzPidPath())

	sig := make(chan os.Signal, 2)
	signalNotifyStop(sig)
	stop := make(chan struct{})

	bridgeErr := make(chan error, 1)
	go func() { bridgeErr <- serveBridge(stop) }()

	// Publish the guest's ports on the host loopback, so the URLs the stack
	// prints actually resolve from the Mac's browser.
	forwards := startForwards()
	defer func() {
		for _, f := range forwards {
			_ = f.Close()
		}
	}()

	fmt.Fprintf(os.Stderr, "[sklp] macos vm: up (pid %d, socket %s)\n", os.Getpid(), vzSocketPath())

	vmGone := make(chan struct{})
	go watchVM(stop, vmGone)
	go syncGuestClock(stop)

	select {
	case <-sig:
		fmt.Fprintln(os.Stderr, "[sklp] macos vm: stopping")
	case err := <-bridgeErr:
		if err != nil {
			fmt.Fprintf(os.Stderr, "[sklp] macos vm: bridge failed: %v\n", err)
		}
	case <-vmGone:
		fmt.Fprintln(os.Stderr, "[sklp] macos vm: guest stopped — releasing host ports")
	}
	close(stop)
	shutdownVM()
	return nil
}

// clockSyncInterval paces the guest wall-clock re-sync. The guest has no RTC
// and freezes across host sleeps — after a night of sleep it woke ten hours
// behind, making apt reject repository signatures as not yet valid. One
// session a minute keeps it within drift no build tool notices.
const clockSyncInterval = time.Minute

// syncGuestClock periodically sets the guest clock to the host's over a
// normal agent session. Failures are ignored: the guest may still be booting,
// and the next tick retries.
func syncGuestClock(stop <-chan struct{}) {
	t := time.NewTicker(clockSyncInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			conn, err := dialGuest()
			if err != nil {
				continue
			}
			epoch := strconv.FormatInt(time.Now().Unix(), 10)
			_, _ = runSession(conn, vzRequest{
				Cwd:  "/",
				Argv: []string{"sh", "-c", "date -u -s @" + epoch + " >/dev/null"},
			})
			conn.Close()
		}
	}
}

// vmWatchInterval is how often the daemon asks whether the guest is still
// there. Coarse on purpose: this only bounds how long a dead VM's forwards
// stay open, and the daemon is otherwise idle.
const vmWatchInterval = time.Second

// watchVM closes gone once the guest is no longer running, so the daemon tears
// its host forwards down with it. Without this the daemon only ever woke on a
// signal or a bridge failure: a VM that died — or a configuration the
// hypervisor rejected outright, which still ran startForwards first — left
// every forwarded port listening on the host with nothing behind it, answering
// each connection with an immediate close (ERR_EMPTY_RESPONSE) instead of the
// connection refused that would name the real problem.
func watchVM(stop <-chan struct{}, gone chan<- struct{}) {
	t := time.NewTicker(vmWatchInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if vmStopped() {
				close(gone)
				return
			}
		}
	}
}
