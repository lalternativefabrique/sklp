//go:build linux

package spacenet

import (
	"os/exec"
	"runtime"

	"golang.org/x/sys/unix"
)

// startPinned runs cmd.Start() on a goroutine locked to its OS thread, and
// keeps that thread alive until the child exits. Pdeathsig is armed against
// the THREAD that issued the clone, not the process: if the Go runtime
// destroyed that thread while the child still ran, the kernel would kill the
// child. Once the child has exited the goroutine returns still locked, so the
// runtime destroys the thread instead of leaking it. The exit is observed on a
// pidfd, which never reaps: the caller's Wait still collects the status.
func startPinned(cmd *exec.Cmd) error {
	errCh := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		err := cmd.Start()
		if err != nil {
			errCh <- err
			return
		}
		pidfd, openErr := unix.PidfdOpen(cmd.Process.Pid, 0)
		errCh <- nil
		if openErr != nil {
			select {}
		}
		defer unix.Close(pidfd)
		if !waitPidfdExit(pidfd) {
			select {}
		}
	}()
	return <-errCh
}

func waitPidfdExit(pidfd int) bool {
	fds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
	for {
		n, err := unix.Poll(fds, -1)
		switch {
		case err == unix.EINTR:
		case err != nil:
			return false
		case n > 0:
			return true
		}
	}
}
