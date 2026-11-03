//go:build linux

package containerinit

import (
	"os"
	"syscall"
	"testing"
	"time"
)

func TestRunReturnsChildExitCode(t *testing.T) {
	if got := Run([]string{"sh", "-c", "exit 3"}); got != 3 {
		t.Fatalf("Run = %d, want 3", got)
	}
}

func TestRunReportsSignaledChild(t *testing.T) {
	if got := Run([]string{"sh", "-c", "kill -KILL $$"}); got != 137 {
		t.Fatalf("Run = %d, want 137", got)
	}
}

func TestRunUnknownCommand(t *testing.T) {
	if got := Run([]string{"/nonexistent/binary"}); got != 127 {
		t.Fatalf("Run = %d, want 127", got)
	}
}

func TestRunReapsOrphansWhileTheChildRuns(t *testing.T) {
	done := make(chan int, 1)
	go func() { done <- Run([]string{"sh", "-c", "(sleep 0.05 &); sleep 0.5; exit 0"}) }()
	select {
	case got := <-done:
		if got != 0 {
			t.Fatalf("Run = %d, want 0", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return once its child exited")
	}
	var ws syscall.WaitStatus
	pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
	if err != syscall.ECHILD {
		t.Fatalf("Wait4 after Run = (%d, %v), want ECHILD: the orphan was not reaped", pid, err)
	}
}

func TestRunForwardsSignals(t *testing.T) {
	done := make(chan int, 1)
	go func() {
		done <- Run([]string{"sh", "-c", `trap 'exit 7' TERM; while :; do sleep 0.05; done`})
	}()
	time.Sleep(300 * time.Millisecond)
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got != 7 {
			t.Fatalf("Run = %d, want 7 (child's TERM trap)", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM was not forwarded to the child")
	}
}
