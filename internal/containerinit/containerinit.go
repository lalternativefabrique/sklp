// Package containerinit is sklp's PID 1 for a deployed service: it execs the
// service command as its child, forwards the signals it receives, reaps every
// orphan the container reparents to it, and exits with the child's status.
package containerinit

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

var forwarded = []os.Signal{
	syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGHUP,
	syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGWINCH, syscall.SIGCONT,
}

// Run starts argv under this process and blocks until that child exits.
// Orphans are reaped as they die; the child's exit code (or 128+signal) is
// returned. 127 means the command could not be started.
func Run(argv []string) int {
	if len(argv) > 0 && argv[0] == "--" {
		argv = argv[1:]
	}
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "init: missing command")
		return 2
	}
	setChildSubreaper()

	sigs := make(chan os.Signal, 64)
	signal.Notify(sigs, append(forwarded, syscall.SIGCHLD)...)
	defer signal.Stop(sigs)

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "init: %v\n", err)
		return 127
	}
	child := cmd.Process.Pid

	for s := range sigs {
		if s != syscall.SIGCHLD {
			_ = syscall.Kill(child, s.(syscall.Signal))
			continue
		}
		if code, exited := reap(child); exited {
			return code
		}
	}
	return 0
}

// reap collects every exited child; exited reports whether pid was among
// them, with its exit code.
func reap(pid int) (code int, exited bool) {
	for {
		var ws syscall.WaitStatus
		got, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if got <= 0 || err != nil {
			return code, exited
		}
		if got != pid {
			continue
		}
		exited = true
		if ws.Signaled() {
			code = 128 + int(ws.Signal())
		} else {
			code = ws.ExitStatus()
		}
	}
}
