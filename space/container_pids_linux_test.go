//go:build linux

package space

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// fakeRuncForPID writes a runc stand-in whose `state` reports a real host
// process: running while it is alive, stopped once it is gone.
func fakeRuncForPID(t *testing.T, pid int) (binPath, stateDir string) {
	t.Helper()
	binPath = filepath.Join(t.TempDir(), "fake-runc")
	script := fmt.Sprintf(`#!/bin/sh
case "$*" in
  *" state "*) if kill -0 %d 2>/dev/null; then echo '{"status":"running","pid":%d}'; else echo '{"status":"stopped","pid":%d}'; fi ;;
esac
exit 0
`, pid, pid, pid)
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binPath, t.TempDir()
}

func TestProcessTree_IncludesDescendants(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 30 & sleep 30 & wait")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	time.Sleep(200 * time.Millisecond)

	tree := processTree(cmd.Process.Pid)
	if len(tree) != 3 {
		t.Fatalf("processTree = %v, want the shell and its two sleeps", tree)
	}
	if tree[0] != cmd.Process.Pid {
		t.Fatalf("tree root = %d, want %d", tree[0], cmd.Process.Pid)
	}
}

// A shell that shrugs off SIGTERM but exits once its child is gone stands in
// for a pid-namespace init with no handler: signalling init alone would sit
// out the whole grace period, signalling its children ends it at once.
func TestKillContainerWithin_ReachesChildrenOfADeafInit(t *testing.T) {
	cmd := exec.Command("sh", "-c", `sleep 30 & trap ":" TERM; while kill -0 $! 2>/dev/null; do sleep 0.1; done`)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	time.Sleep(200 * time.Millisecond)
	bin, stateDir := fakeRuncForPID(t, cmd.Process.Pid)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	start := time.Now()
	killContainerWithin(bin, stateDir, "deaf-init", 10*time.Second)
	elapsed := time.Since(start)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the shell is still alive: its child never received SIGTERM")
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("killContainerWithin took %v — it waited for the grace period instead of the child's exit", elapsed)
	}
}
