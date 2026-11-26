package devstack

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	detachedEnv      = "SKLP_DEV_DETACHED"
	stackReadyBanner = "✓ stack ready"
)

func runningDetached() bool { return os.Getenv(detachedEnv) == "1" }

func detachLogPath(repoAbs, stack string) string {
	return filepath.Join(stateDir(), "dev-"+devScopeID(repoAbs, stack)+".log")
}

// withoutDetachFlag drops -d from the launch argv so the background child runs
// the plain foreground supervisor.
func withoutDetachFlag(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		switch strings.TrimLeft(a, "-") {
		case "d", "d=true", "detach", "detach=true":
			if strings.HasPrefix(a, "-") {
				continue
			}
		}
		out = append(out, a)
	}
	return out
}

// runDetached starts the supervisor in its own session and follows its output
// until the stack is ready, then returns while it keeps running. On macOS the
// child is also what holds the mux port forward, so it must be the process
// that relays into the VM.
func runDetached(repoAbs, stack string, args []string) int {
	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "sklp dev: %v\n", err)
		return 1
	}
	logPath := detachLogPath(repoAbs, stack)
	logFile, err := os.Create(logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sklp dev: %v\n", err)
		return 1
	}
	self, err := os.Executable()
	if err != nil {
		_ = logFile.Close()
		fmt.Fprintf(os.Stderr, "sklp dev: %v\n", err)
		return 1
	}

	cmd := exec.Command(self, append([]string{"dev"}, withoutDetachFlag(args)...)...)
	cmd.Env = append(os.Environ(), detachedEnv+"=1")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	_ = logFile.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "sklp dev: start background stack: %v\n", err)
		return 1
	}

	exited := make(chan int, 1)
	go func() {
		err := cmd.Wait()
		var exitErr *exec.ExitError
		switch {
		case err == nil:
			exited <- 0
		case errors.As(err, &exitErr):
			exited <- exitErr.ExitCode()
		default:
			exited <- 1
		}
	}()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)

	switch outcome, code := followUntilReady(logPath, os.Stdout, exited, sigs); outcome {
	case startupReady:
		fmt.Printf("sklp dev: %s running in the background (pid %d)\n  logs: tail -f %s\n  stop: sklp dev down %s\n",
			stack, cmd.Process.Pid, logPath, stack)
		return 0
	case startupInterrupted:
		_ = cmd.Process.Signal(syscall.SIGTERM)
		<-exited
		return 130
	default:
		fmt.Fprintf(os.Stderr, "sklp dev: %s stopped during startup (exit %d) — log: %s\n", stack, code, logPath)
		if code == 0 {
			code = 1
		}
		return code
	}
}

type startupOutcome int

const (
	startupReady startupOutcome = iota
	startupExited
	startupInterrupted
)

// followUntilReady copies the child's log to out as it grows and reports how
// startup ended: the ready banner, the child exiting, or the user interrupting.
func followUntilReady(logPath string, out io.Writer, exited <-chan int, sigs <-chan os.Signal) (startupOutcome, int) {
	f, err := os.Open(logPath)
	if err != nil {
		return startupExited, 1
	}
	defer f.Close()
	r := bufio.NewReader(f)
	var partial string
	drain := func() bool {
		for {
			chunk, err := r.ReadString('\n')
			if err != nil {
				partial += chunk
				return false
			}
			line := partial + chunk
			partial = ""
			_, _ = io.WriteString(out, line)
			if strings.Contains(line, stackReadyBanner) {
				return true
			}
		}
	}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if drain() {
			return startupReady, 0
		}
		select {
		case code := <-exited:
			if drain() {
				return startupReady, 0
			}
			if partial != "" {
				_, _ = io.WriteString(out, partial+"\n")
			}
			return startupExited, code
		case <-sigs:
			return startupInterrupted, 130
		case <-tick.C:
		}
	}
}
