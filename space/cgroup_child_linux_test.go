//go:build linux

package space

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fixtureSlice(t *testing.T) string {
	t.Helper()
	slice := t.TempDir()
	prev := childCgroupSlice
	childCgroupSlice = func() string { return slice }
	t.Cleanup(func() { childCgroupSlice = prev })
	return slice
}

func TestClearChildCgroup_RemovesAnEmptyCgroup(t *testing.T) {
	slice := fixtureSlice(t)
	empty := filepath.Join(slice, "svc-empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	clearChildCgroup("svc-empty")
	clearChildCgroup("svc-missing")
	if _, err := os.Stat(empty); !os.IsNotExist(err) {
		t.Fatal("an empty cgroup must be removed")
	}
}

// A cgroup still holding a previous run's processes is emptied through
// cgroup.kill before the service starts again, so runc does not meet a
// container it believes is running.
func TestClearChildCgroup_KillsWhatAPreviousRunLeft(t *testing.T) {
	slice := fixtureSlice(t)
	busy := filepath.Join(slice, "svc-busy")
	if err := os.MkdirAll(busy, 0o755); err != nil {
		t.Fatal(err)
	}
	procs := filepath.Join(busy, "cgroup.procs")
	kill := filepath.Join(busy, "cgroup.kill")
	if err := os.WriteFile(procs, []byte("4242\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	go func() {
		for i := 0; i < 100; i++ {
			if b, err := os.ReadFile(kill); err == nil && string(b) == "1" {
				_ = os.Remove(kill)
				_ = os.Remove(procs)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	clearChildCgroup("svc-busy")
	if _, err := os.Stat(busy); !os.IsNotExist(err) {
		t.Fatalf("the cgroup must be emptied and removed: %v", err)
	}
}
