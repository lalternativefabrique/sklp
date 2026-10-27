//go:build linux

package spacenet

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A holder killed by the host (the next rollout of its service does exactly
// that) must be reaped by the worker that forked it, or it stays a zombie
// counted against the unit's TasksMax until fork fails.
func TestOverlayHolder_IsReapedOnceKilled(t *testing.T) {
	skipUnlessRootlessNetns(t)
	cn, err := NewSpaceNet("reap", "10.96.0.0/24")
	if err != nil {
		t.Skipf("NewSpaceNet: %v", err)
	}
	defer cn.Close()
	base := t.TempDir()
	lower, upper, work, merged := filepath.Join(base, "lower"), filepath.Join(base, "upper"), filepath.Join(base, "work"), filepath.Join(base, "merged")
	for _, d := range []string{lower, upper, work, merged} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pid, err := cn.SpawnOverlay(lower, upper, work, merged)
	if err != nil {
		t.Fatalf("SpawnOverlay: %v", err)
	}
	waitOverlayMounted(t, pid, merged)

	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		st, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return
		}
		if !strings.Contains(string(st), ") Z ") {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		time.Sleep(20 * time.Millisecond)
	}
	st, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	t.Fatalf("holder %d was not reaped by its worker: %s", pid, strings.TrimSpace(string(st)))
}
