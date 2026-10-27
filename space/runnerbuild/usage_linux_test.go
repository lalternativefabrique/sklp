//go:build linux

package runnerbuild

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func entryDir(cacheRoot, hash string) string {
	return filepath.Dir(layoutFor(cacheRoot, hash).rootfs)
}

// deadPid returns a pid no process holds. A hardcoded huge number will not do:
// past /proc/sys/kernel/pid_max the kernel answers EINVAL, not ESRCH, which is
// not the "process is gone" case under test.
func deadPid(t *testing.T) int {
	t.Helper()
	for pid := 2; pid < 1<<22; pid++ {
		if pid != os.Getpid() && !processAlive(pid) {
			return pid
		}
	}
	t.Skip("no free pid on this machine")
	return 0
}

// With --concurrency > 1 several runs share one rootfs. A single-writer lock
// would have the first to finish release a pin the others still depend on, so
// the collector could reclaim the tree from under them: the lock must hold
// until the LAST holder is gone.
func TestLock_HoldsUntilLastHolderReleases(t *testing.T) {
	root := t.TempDir()
	const hash = "deadbeef"
	dir := entryDir(root, hash)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	releaseA := Lock(root, hash)
	// A second holder that is NOT this process, standing in for a concurrent
	// run: pid 1 always exists, so it reads as live.
	if err := appendPid(filepath.Join(dir, lockFn), 1); err != nil {
		t.Fatal(err)
	}

	releaseA()
	if !IsLocked(dir) {
		t.Fatal("entry reported free while a second live holder remains")
	}

	removePid(filepath.Join(dir, lockFn), 1)
	if IsLocked(dir) {
		t.Error("entry still locked after every holder released")
	}
}

// A pid recorded by a run that was SIGKILLed never runs its release. Rewriting
// the lock drops it, so a crash cannot pin a multi-GiB tree forever.
func TestLock_DropsDeadPids(t *testing.T) {
	root := t.TempDir()
	const hash = "cafebabe"
	dir := entryDir(root, hash)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, lockFn), []byte(strconv.Itoa(deadPid(t))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if IsLocked(dir) {
		t.Fatal("a lock naming only a dead pid reported the entry live")
	}

	release := Lock(root, hash)
	defer release()
	if pids := readPids(filepath.Join(dir, lockFn)); len(pids) != 1 || pids[0] != os.Getpid() {
		t.Errorf("lock = %v, want only this pid (the dead one must be dropped)", pids)
	}
}

// A cache HIT touches no file in the entry, so directory mtime cannot serve as
// the LRU key: the rootfs a pipeline reuses on every run would look as cold as
// a stranded one. MarkUsed is what keeps the hot entry hot.
func TestMarkUsed_StampsOnReuse(t *testing.T) {
	root := t.TempDir()
	const hash = "feedface"
	if err := os.MkdirAll(entryDir(root, hash), 0o755); err != nil {
		t.Fatal(err)
	}

	want := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	MarkUsed(root, hash, want)

	got, ok := ReadUsage(entryDir(root, hash))
	if !ok {
		t.Fatal("no usage recorded")
	}
	if !got.LastUsed.UTC().Truncate(time.Second).Equal(want) {
		t.Errorf("LastUsed = %s, want %s", got.LastUsed, want)
	}
	if got.Hash != hash {
		t.Errorf("Hash = %q, want %q", got.Hash, hash)
	}
}
