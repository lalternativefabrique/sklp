//go:build linux

package runnerbuild

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	usageFn = ".usage"
	lockFn  = ".lock"
)

// Usage records when a runner rootfs was last handed to a run. The build hash
// is content-derived and never invertible back to a recipe, so a collector
// looking at runner/<hash> alone cannot tell a rootfs built an hour ago from
// one stranded by a space.yaml bump months back. Directory mtime does not
// answer it either: a cache HIT touches nothing, so the entry a pipeline uses
// on every run looks as cold as a dead one. This sidecar is the only record of
// the LRU key, so it is rewritten on every Build call, hit or miss.
type Usage struct {
	Hash     string    `json:"hash"`
	LastUsed time.Time `json:"lastUsed"`
	// Size is the built rootfs's byte size, measured once at build time. A
	// finished rootfs is never written again — the container mounts it
	// read-only and its writes land in a per-service overlay — so the figure
	// cannot drift, and caching it keeps the end-of-run collector off a
	// WalkDir of ~75k inodes per entry (~0.5s per pass on a warm NVMe, worse
	// on a CI node's disk with a cold page cache).
	Size int64 `json:"size"`
}

// MarkUsed stamps the rootfs keyed by hash as used at now, carrying forward the
// size already recorded so a cache hit does not re-measure the tree.
// Best-effort: a failed stamp costs the entry its LRU position (it falls back
// to mtime), never the build.
func MarkUsed(cacheRoot, hash string, now time.Time) {
	dir := filepath.Dir(layoutFor(cacheRoot, hash).rootfs)
	var size int64
	if prev, ok := ReadUsage(dir); ok {
		size = prev.Size
	}
	writeUsage(dir, Usage{Hash: hash, LastUsed: now, Size: size})
}

// MarkBuilt stamps a freshly built rootfs, measuring it once. Separate from
// MarkUsed because this is the only moment the size is not yet known.
func MarkBuilt(cacheRoot, hash string, now time.Time) {
	dir := filepath.Dir(layoutFor(cacheRoot, hash).rootfs)
	writeUsage(dir, Usage{Hash: hash, LastUsed: now, Size: treeBytes(dir)})
}

func writeUsage(dir string, u Usage) {
	blob, err := json.Marshal(u)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, usageFn), blob, 0o600)
}

func treeBytes(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, ferr := d.Info(); ferr == nil {
			total += fi.Size()
		}
		return nil
	})
	return total
}

// ReadUsage returns the recorded usage for a runner cache entry directory.
func ReadUsage(entryDir string) (Usage, bool) {
	blob, err := os.ReadFile(filepath.Join(entryDir, usageFn))
	if err != nil {
		return Usage{}, false
	}
	var u Usage
	if err := json.Unmarshal(blob, &u); err != nil {
		return Usage{}, false
	}
	return u, true
}

// Lock pins the rootfs keyed by hash for the lifetime of the returned release.
// A rootfs is bind-mounted read-only into every container of a run that uses
// it, so unlinking it mid-run strands the mount on a vanished source and the
// container's own binaries disappear underneath it. buildLocks cannot serve
// here: it is in-process, and the collector runs from another `sklp cache gc`
// process entirely.
func Lock(cacheRoot, hash string) func() {
	dir := filepath.Dir(layoutFor(cacheRoot, hash).rootfs)
	path := filepath.Join(dir, lockFn)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return func() {}
	}
	if err := appendPid(path, os.Getpid()); err != nil {
		return func() {}
	}
	return func() { removePid(path, os.Getpid()) }
}

// IsLocked reports whether any pid recorded against the entry is still alive.
// The lock file holds one pid per line rather than a single value because
// --concurrency lets several runs share one rootfs: a single-writer lock would
// have the first to finish release a pin the others still depend on.
func IsLocked(entryDir string) bool {
	for _, pid := range readPids(filepath.Join(entryDir, lockFn)) {
		if processAlive(pid) {
			return true
		}
	}
	return false
}

func readPids(path string) []int {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []int
	for _, line := range strings.Split(string(blob), "\n") {
		if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && pid > 0 {
			out = append(out, pid)
		}
	}
	return out
}

// appendPid rewrites the lock with the live pids plus this one. Dropping dead
// pids on every write keeps a run killed with SIGKILL — which never runs its
// release — from pinning the entry forever.
func appendPid(path string, pid int) error {
	var b strings.Builder
	for _, p := range readPids(path) {
		if p != pid && processAlive(p) {
			b.WriteString(strconv.Itoa(p))
			b.WriteByte('\n')
		}
	}
	b.WriteString(strconv.Itoa(pid))
	b.WriteByte('\n')
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

func removePid(path string, pid int) {
	var b strings.Builder
	for _, p := range readPids(path) {
		if p != pid && processAlive(p) {
			b.WriteString(strconv.Itoa(p))
			b.WriteByte('\n')
		}
	}
	if b.Len() == 0 {
		_ = os.Remove(path)
		return
	}
	_ = os.WriteFile(path, []byte(b.String()), 0o600)
}

// processAlive reports whether pid names a live process. Signal 0 performs the
// existence and permission checks without delivering anything, so a nil error
// means alive — but so does EPERM: the signal was refused precisely because a
// process exists and belongs to someone else. Only ESRCH proves absence.
// Reading EPERM as "dead" would let the collector unlink a rootfs still mounted
// under another user's run.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return !errors.Is(proc.Signal(syscall.Signal(0)), syscall.ESRCH)
}
