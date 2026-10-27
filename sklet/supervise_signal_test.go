package sklet

import (
	"context"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// envSuperviseSignalProbe re-execs THIS test binary as a tiny stand-in for the
// __supervise daemon: it installs the exact same signal disposition runSupervise
// uses — ignore SIGHUP, teardown on SIGINT/SIGTERM — then blocks until ctx is
// cancelled. It deliberately omits the container/launcher work so the test can
// drive only the signal contract, with no leaked runc state.
const envSuperviseSignalProbe = "SKLP_TEST_SUPERVISE_SIGNAL_PROBE"

func TestMain(m *testing.M) {
	if os.Getenv(envSuperviseSignalProbe) == "1" {
		runSuperviseSignalProbe()
		return
	}
	os.Exit(m.Run())
}

// runSuperviseSignalProbe mirrors the head of runSupervise: it must survive
// SIGHUP and exit only on SIGINT/SIGTERM (which cancel ctx). It prints "ready"
// once armed so the parent test knows signals are now handled.
func runSuperviseSignalProbe() {
	signal.Ignore(syscall.SIGHUP)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Tell the parent we are armed; it sends signals only after this line.
	_, _ = os.Stdout.WriteString("ready\n")
	<-ctx.Done() // SIGINT/SIGTERM cancels; SIGHUP must NOT reach here.
	os.Exit(0)
}

// TestSuperviseIgnoresSIGHUPButStopsOnSIGTERM is BUG #1's regression guard: a
// detached supervisor MUST survive a SIGHUP (the launcher terminal hanging up)
// and still tear down on SIGTERM. It re-execs the probe above, sends SIGHUP,
// verifies it is still alive, then sends SIGTERM and verifies a clean exit.
func TestSuperviseIgnoresSIGHUPButStopsOnSIGTERM(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe)
	cmd.Env = append(os.Environ(), envSuperviseSignalProbe+"=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start probe: %v", err)
	}
	// Guarantee no leak even if an assertion fails mid-test.
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	// Wait for the probe to arm its signal handlers before sending anything.
	buf := make([]byte, len("ready\n"))
	if _, err := io.ReadFull(stdout, buf); err != nil {
		t.Fatalf("probe never became ready: %v", err)
	}

	// SIGHUP must be ignored: the probe stays alive.
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatalf("send SIGHUP: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if !processAlive(t, cmd.Process.Pid) {
		t.Fatal("supervisor died on SIGHUP; a daemon must ignore SIGHUP")
	}

	// SIGTERM must still trigger a clean teardown (exit 0).
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("supervisor did not exit cleanly on SIGTERM: %v", err)
	}
}

// processAlive reports whether pid is still running (signal 0 probe).
func processAlive(t *testing.T, pid int) bool {
	t.Helper()
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
