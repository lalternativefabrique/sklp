//go:build darwin && arm64 && cgo

package limarelay

/*
#cgo CFLAGS: -x objective-c -fmodules -fobjc-arc
#cgo LDFLAGS: -framework Virtualization -framework Foundation
#include <stdlib.h>
int sklp_vz_start(const char *kernelPath, const char *diskPath, const char *cmdline,
                  const char *shareDir, const char *shareTag,
                  const char *cacheDir, const char *cacheTag,
                  int cpuCount, unsigned long long memBytes,
                  int consoleFd, char *errBuf, int errLen);
int sklp_vz_connect(unsigned int port, char *errBuf, int errLen);
int sklp_vz_wait(int timeoutSec);
int sklp_vz_stopped(void);
void sklp_vz_stop(void);
*/
import "C"

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/term"
)

const (
	vzShareTag       = "hostshare"
	vzCacheTag       = "sktcache"
	vzAgentPort      = 1024
	vzBootTimeoutSec = 120
)

// vzRequest is what the guest agent reads on the vsock connection.
type vzRequest struct {
	Cwd  string   `json:"cwd"`
	Env  []string `json:"env"`
	Argv []string `json:"argv"`
	TTY  bool     `json:"tty"`  // allocate a pty in the guest
	Cols int      `json:"cols"` // initial window size when TTY
	Rows int      `json:"rows"`
}

// vzReply is the agent's terminal message once the command has finished.
type vzReply struct {
	Exit int    `json:"exit"`
	Err  string `json:"err,omitempty"`
}

func cs(s string) *C.char {
	if s == "" {
		return nil
	}
	return C.CString(s)
}

// startVM brings the machine up and leaves it running. The caller owns its
// lifetime: the VM dies with the process that created it, so whoever calls
// this must stay alive for as long as the VM should exist.
func (v vzProvider) startVM() error {
	console, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if os.Getenv("SKLP_VZ_DEBUG") != "" {
		console = os.Stderr
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	cacheDir := filepath.Join(home, ".cache", "skt")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return err
	}

	errBuf := (*C.char)(C.malloc(512))
	defer C.free(unsafe.Pointer(errBuf))

	cpus := runtime.NumCPU()
	if cpus > 8 {
		cpus = 8
	}
	mem := uint64(4) << 30
	if s := os.Getenv("SKLP_VZ_MEMORY_GB"); s != "" {
		if n, convErr := strconv.Atoi(s); convErr == nil && n > 0 {
			mem = uint64(n) << 30
		}
	}

	cKernel, cDisk := cs(v.kernel), cs(v.rootfs)
	cCmd := cs("console=hvc0 root=/dev/vda rw quiet init=/init sklp.home=" + home +
		" sklp.now=" + strconv.FormatInt(time.Now().Unix(), 10))
	cShare, cShareTag := cs(home), cs(vzShareTag)
	cCache, cCacheTag := cs(cacheDir), cs(vzCacheTag)
	defer func() {
		for _, p := range []*C.char{cKernel, cDisk, cCmd, cShare, cShareTag, cCache, cCacheTag} {
			C.free(unsafe.Pointer(p))
		}
	}()

	rc := C.sklp_vz_start(cKernel, cDisk, cCmd, cShare, cShareTag, cCache, cCacheTag,
		C.int(cpus), C.ulonglong(mem), C.int(console.Fd()), errBuf, 512)
	if rc != 0 {
		msg := C.GoString(errBuf)
		// The framework reports a disk already opened by another VM as a plain
		// "invalid" configuration, which reads like a bug in the config rather
		// than the one thing it actually means: something else already holds
		// this rootfs.
		if strings.Contains(msg, "storage device attachment is invalid") {
			return fmt.Errorf("vz: %s is already open — another sklp holds this VM", v.rootfs)
		}
		return fmt.Errorf("vz: %s", msg)
	}
	return nil
}

// bootPersistent starts the VM and waits for the guest agent to answer, then
// returns leaving both up. Used by the daemon, which then parks on a signal.
func (v vzProvider) bootPersistent() error {
	if err := v.startVM(); err != nil {
		return err
	}
	errBuf := (*C.char)(C.malloc(512))
	defer C.free(unsafe.Pointer(errBuf))

	conn, err := vzDial(errBuf)
	if err != nil {
		shutdownVM()
		return err
	}
	// The probe connection is closed immediately: it only proves the agent is
	// serving. Real sessions dial their own.
	conn.Close()
	return nil
}

// shutdownVM asks the guest to power off. Best-effort: the process exiting
// takes the VM with it regardless.
func shutdownVM() { C.sklp_vz_stop() }

// vmStopped reports whether the guest is gone — powered off, failed, or never
// started. The daemon polls it to tear its host forwards down with the VM
// instead of leaving them listening on a guest that cannot answer.
func vmStopped() bool { return C.sklp_vz_stopped() != 0 }

// runSession drives one command over an already-open connection to the guest
// agent, whatever carried it: a vsock the caller owns, or the daemon's UNIX
// socket. Returns the guest's exit status.
func runSession(conn io.ReadWriter, req vzRequest) (int, error) {
	if w, h, terr := term.GetSize(int(os.Stdin.Fd())); terr == nil {
		req.TTY, req.Cols, req.Rows = true, w, h
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return 0, fmt.Errorf("vz: send request: %w", err)
	}

	if req.TTY {
		// Raw mode so keystrokes reach the guest program unbuffered and its
		// own rendering is not fought over by the host terminal's line
		// discipline. Restored before this function returns, including on a
		// guest crash, or the shell is left unusable.
		prev, terr := term.MakeRaw(int(os.Stdin.Fd()))
		if terr == nil {
			defer term.Restore(int(os.Stdin.Fd()), prev)
		}
		stopWinch := forwardResize(conn)
		defer stopWinch()
	}

	stopStdin := forwardStdin(conn)
	defer stopStdin()

	stop := forwardSignals(conn)
	defer stop()

	return vzPump(conn)
}

// vzDial waits for the guest agent to bind its vsock port. The guest boots in
// about a second, but a cold page cache can make that several.
func vzDial(errBuf *C.char) (*os.File, error) {
	for i := 0; i < vzBootTimeoutSec*10; i++ {
		fd := C.sklp_vz_connect(C.uint(vzAgentPort), errBuf, 512)
		if fd >= 0 {
			return os.NewFile(uintptr(fd), "vsock"), nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fmt.Errorf("vz: guest agent did not answer on vsock:%d (%s)",
		vzAgentPort, C.GoString(errBuf))
}

// vzPump relays framed guest output to stdout/stderr until the agent sends its
// final reply, which carries the exit status.
func vzPump(conn io.Reader) (int, error) {
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		switch line[0] {
		case 'P':
			// A pty session carries control sequences and partial lines, so it
			// is base64-encoded: decode and write the bytes through untouched.
			if b, err := base64.StdEncoding.DecodeString(string(line[1:])); err == nil {
				os.Stdout.Write(b)
			}
		case '1':
			os.Stdout.Write(append(line[1:], '\n'))
		case '2':
			os.Stderr.Write(append(line[1:], '\n'))
		case 'X':
			var r vzReply
			if err := json.Unmarshal(line[1:], &r); err != nil {
				return 0, fmt.Errorf("vz: bad reply: %w", err)
			}
			if r.Err != "" {
				return r.Exit, fmt.Errorf("vz: guest: %s", r.Err)
			}
			return r.Exit, nil
		}
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("vz: guest stream: %w", err)
	}
	return 0, fmt.Errorf("vz: guest closed the connection without reporting an exit status")
}

// forwardStdin streams the host's stdin to the guest as base64 "I" frames.
// Encoding keeps the framing line-oriented, so a keystroke that happens to be
// a newline cannot be mistaken for a frame boundary.
func forwardStdin(conn io.Writer) func() {
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 4096)
		for {
			select {
			case <-done:
				return
			default:
			}
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				fmt.Fprintf(conn, "I%s\n", base64.StdEncoding.EncodeToString(buf[:n]))
			}
			if err != nil {
				// EOF closes the guest's stdin so a program reading to the end
				// (a pipe, `sklp run < file`) terminates instead of hanging.
				fmt.Fprint(conn, "E\n")
				return
			}
		}
	}()
	return func() { close(done) }
}

// forwardResize relays SIGWINCH so a resized host terminal resizes the guest
// pty, which full-screen programs like the cockpit redraw against.
func forwardResize(conn io.Writer) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ch:
				if w, h, err := term.GetSize(int(os.Stdin.Fd())); err == nil {
					fmt.Fprintf(conn, "W%d %d\n", w, h)
				}
			case <-done:
				return
			}
		}
	}()
	return func() { signal.Stop(ch); close(done) }
}

// forwardSignals relays interactive signals to the guest so Ctrl-C reaches the
// command actually doing the work, mirroring what an exec'd limactl gives for
// free.
func forwardSignals(conn io.Writer) func() {
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-ch:
				n := 2
				if s == syscall.SIGTERM {
					n = 15
				} else if s == syscall.SIGQUIT {
					n = 3
				}
				fmt.Fprintf(conn, "S%d\n", n)
			case <-done:
				return
			}
		}
	}()
	return func() { signal.Stop(ch); close(done) }
}

// dialGuest opens one vsock session to the guest agent. Callable only from the
// process holding the VM, which is why the daemon fronts it with a UNIX
// socket for everyone else.
func dialGuest() (net.Conn, error) { return dialGuestPort(vzAgentPort) }

// dialGuestPort opens a vsock session to one guest port. Callable only from
// the process holding the VM, which is why the daemon fronts both the command
// channel and the port forwards for everyone else.
func dialGuestPort(port int) (net.Conn, error) {
	errBuf := (*C.char)(C.malloc(512))
	defer C.free(unsafe.Pointer(errBuf))

	fd := C.sklp_vz_connect(C.uint(port), errBuf, 512)
	if fd < 0 {
		return nil, fmt.Errorf("vsock: %s", C.GoString(errBuf))
	}
	f := os.NewFile(uintptr(fd), "vsock")
	conn, err := net.FileConn(f)
	// FileConn dups the descriptor, so the original is closed either way.
	f.Close()
	if err != nil {
		return nil, err
	}
	return conn, nil
}
