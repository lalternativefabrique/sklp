package space

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	toolcacheDirName  = "ci-toolcache"
	toolcacheShared   = "shared"
	toolcacheOriginFn = ".origin"
)

// ToolcacheOrigin records which checkout produced a ci-toolcache bucket. The
// bucket name embeds sha256(abs(repoPath))[:4] (see ciSpaceName), which is not
// invertible: without this sidecar a collector can only guess a bucket's owner
// by hashing candidate paths, and a path it fails to enumerate looks exactly
// like a dead one. Writing the origin at bucket-creation time turns the
// liveness question into a stat(2).
type ToolcacheOrigin struct {
	Repo    string    `json:"repo"`
	Task    string    `json:"task"`
	LastRun time.Time `json:"lastRun"`
}

// writeToolcacheOrigin stamps a bucket with the checkout that owns it. Called
// on every run so LastRun doubles as the LRU key — bucket mtime is not usable
// for that, since a build writing into go-build/ bumps it regardless of which
// task ran.
func writeToolcacheOrigin(bucketDir, repoPath, taskName string, now time.Time) error {
	abs, err := filepath.Abs(repoPath)
	if err != nil {
		abs = repoPath
	}
	blob, err := json.Marshal(ToolcacheOrigin{Repo: abs, Task: taskName, LastRun: now})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(bucketDir, toolcacheOriginFn), blob, 0o600)
}

func readToolcacheOrigin(bucketDir string) (ToolcacheOrigin, bool) {
	blob, err := os.ReadFile(filepath.Join(bucketDir, toolcacheOriginFn))
	if err != nil {
		return ToolcacheOrigin{}, false
	}
	var o ToolcacheOrigin
	if err := json.Unmarshal(blob, &o); err != nil || o.Repo == "" {
		return ToolcacheOrigin{}, false
	}
	return o, true
}

// defaultToolcacheBudget bounds the total ci-toolcache size when no explicit
// budget is set. Sized to hold a handful of concurrent checkouts' GOCACHE
// (~1-2 GiB each) — high enough that day-to-day work stays warm, low enough
// that a machine cannot silently accumulate tens of gigabytes.
const defaultToolcacheBudget int64 = 12 << 30

// DefaultToolcacheBudget exposes the resolved LRU ceiling so the CLI's default
// matches what an automatic end-of-run collection uses.
func DefaultToolcacheBudget() int64 { return toolcacheBudget() }

// toolcacheBudget resolves the LRU ceiling. SKLP_TOOLCACHE_MAX_BYTES overrides
// the default; 0 disables the LRU pass entirely (orphan collection still runs).
func toolcacheBudget() int64 {
	raw := os.Getenv("SKLP_TOOLCACHE_MAX_BYTES")
	if raw == "" {
		return defaultToolcacheBudget
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return defaultToolcacheBudget
	}
	return n
}

// collectToolcacheAfterRun is the end-of-run reclaim hook. It runs for passing
// and failing pipelines alike and reports only when it actually freed
// something, so a normal run stays silent. Errors are advisory: a bucket the
// collector cannot unlink must never turn a green pipeline red.
func collectToolcacheAfterRun(out io.Writer) {
	res, err := CollectToolcache(ToolcacheGCOpts{Orphans: true, MaxBytes: toolcacheBudget()})
	if err != nil || out == nil || len(res.Removed) == 0 {
		return
	}
	fmt.Fprintf(out, "ci-toolcache: reclaimed %s from %d stale bucket(s)\n",
		humanToolcacheBytes(res.ReclaimedFor), len(res.Removed))
}

func humanToolcacheBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// removeCISpaceState drops a finished CI space's per-space state
// (spaces/<name>/: overlay scratch, runc state, bundles, volumes). Safe only
// because a CI space is rebuilt from .sklp/space.yaml on every run and holds no
// deployment state — never call this on a deploy space. Best-effort: leftover
// state costs disk, not correctness, and must not fail a pipeline. The overlay
// upper holds files written by the container's uid 0, so removal goes through
// the subuid-aware path.
func removeCISpaceState(s *Space) {
	if s == nil || s.scope.standalone() {
		return
	}
	_ = subidRemoveAll(s.scope.spaceDir())
}

// ToolcacheBucket is one ci-toolcache/<task>-<digest> directory as seen by the
// collector.
type ToolcacheBucket struct {
	Name string
	Path string
	Size int64
	// Origin is the recorded owner; Known is false for buckets written before
	// the sidecar existed (or whose sidecar was lost).
	Origin ToolcacheOrigin
	Known  bool
	// Orphan is true when the origin is known AND its checkout is gone. A
	// bucket with Known == false is never an orphan: we cannot prove it.
	Orphan bool
	// Live is true when a space of the same name still exists, i.e. a run may
	// have this bucket bind-mounted right now.
	Live bool
	// LastUsed is Origin.LastRun when known, else the directory mtime.
	LastUsed time.Time
}

// ToolcacheGCOpts selects what a collection pass reclaims.
type ToolcacheGCOpts struct {
	// Orphans removes buckets whose recorded checkout no longer exists.
	Orphans bool
	// MaxBytes, when > 0, evicts least-recently-used buckets until the total
	// falls at or below it. Applied after orphan removal.
	MaxBytes int64
	// DryRun reports what would be removed without unlinking anything.
	DryRun bool
}

// ToolcacheGCResult reports a collection pass.
type ToolcacheGCResult struct {
	Removed      []ToolcacheBucket
	Kept         []ToolcacheBucket
	SkippedLive  []ToolcacheBucket
	ReclaimedFor int64
	Errors       map[string]error
}

// scanToolcacheBuckets lists the buckets under root/ci-toolcache. The shared
// module cache is content-addressed and belongs to no single checkout, so it is
// never a candidate.
func scanToolcacheBuckets(root string) ([]ToolcacheBucket, error) {
	dir := filepath.Join(root, toolcacheDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []ToolcacheBucket
	for _, e := range entries {
		if !e.IsDir() || e.Name() == toolcacheShared {
			continue
		}
		b := ToolcacheBucket{Name: e.Name(), Path: filepath.Join(dir, e.Name())}
		b.Size = dirBytes(b.Path)
		b.Live = bucketIsLocked(b.Path)
		if o, ok := readToolcacheOrigin(b.Path); ok {
			b.Origin, b.Known, b.LastUsed = o, true, o.LastRun
			if _, serr := os.Stat(o.Repo); serr != nil && os.IsNotExist(serr) {
				b.Orphan = true
			}
		}
		if b.LastUsed.IsZero() {
			if fi, ferr := e.Info(); ferr == nil {
				b.LastUsed = fi.ModTime()
			}
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastUsed.Before(out[j].LastUsed) })
	return out, nil
}

const toolcacheLockFn = ".lock"

// lockToolcacheBucket marks a bucket as in use by this process for the duration
// of a run, and returns the release. Liveness cannot be inferred from spaces/:
// Space.Close deliberately leaves per-space state on disk, so a directory there
// says nothing about whether a run is still going. It cannot be inferred from
// the bucket mtime either — a build writes into it constantly. Only a lock
// naming a live pid answers the question the collector actually asks: "would
// unlinking this break a run in flight?"
func lockToolcacheBucket(bucketDir string) func() {
	path := filepath.Join(bucketDir, toolcacheLockFn)
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return func() {}
	}
	return func() { _ = os.Remove(path) }
}

// bucketIsLocked reports whether a bucket is held by a process that still
// exists. A lock naming a dead pid is stale — a run killed with SIGKILL never
// runs its release — and must not pin the bucket forever.
func bucketIsLocked(bucketDir string) bool {
	blob, err := os.ReadFile(filepath.Join(bucketDir, toolcacheLockFn))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(blob)))
	if err != nil || pid <= 0 {
		return false
	}
	return processAlive(pid)
}

// processAlive reports whether pid names a live process. Signal 0 performs the
// existence and permission checks without delivering anything, so a nil error
// means alive — but so does EPERM: the signal was refused precisely because a
// process exists and belongs to someone else. Only ESRCH proves absence.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return !errors.Is(proc.Signal(syscall.Signal(0)), syscall.ESRCH)
}

func dirBytes(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if fi, ferr := d.Info(); ferr == nil {
			total += fi.Size()
		}
		return nil
	})
	return total
}

// CollectToolcache reclaims ci-toolcache buckets. Orphan removal is exact — it
// acts only on a recorded origin whose checkout is gone — while the LRU pass
// bounds the total for checkouts that are all still alive. Buckets belonging to
// a materialised space are never touched: a pipeline may have one bind-mounted.
//
// The shared module cache (ci-toolcache/shared) is out of scope: it is
// content-addressed, shared by every task, and reclaiming it is what
// `sklp clean` is for.
func CollectToolcache(opts ToolcacheGCOpts) (ToolcacheGCResult, error) {
	root, err := cacheRoot()
	if err != nil {
		return ToolcacheGCResult{}, err
	}
	return collectToolcacheIn(root, opts, time.Now())
}

func collectToolcacheIn(root string, opts ToolcacheGCOpts, _ time.Time) (ToolcacheGCResult, error) {
	res := ToolcacheGCResult{Errors: map[string]error{}}
	buckets, err := scanToolcacheBuckets(root)
	if err != nil {
		return res, fmt.Errorf("scan %s: %w", toolcacheDirName, err)
	}

	remove := func(b ToolcacheBucket) bool {
		if opts.DryRun {
			res.Removed = append(res.Removed, b)
			res.ReclaimedFor += b.Size
			return true
		}
		// Buckets hold files written by the container's uid 0, mapped to a
		// subuid on the host — a plain RemoveAll hits EPERM.
		if rerr := subidRemoveAll(b.Path); rerr != nil {
			res.Errors[b.Name] = rerr
			return false
		}
		res.Removed = append(res.Removed, b)
		res.ReclaimedFor += b.Size
		return true
	}

	var survivors []ToolcacheBucket
	for _, b := range buckets {
		if b.Live {
			res.SkippedLive = append(res.SkippedLive, b)
			continue
		}
		if opts.Orphans && b.Orphan {
			if remove(b) {
				continue
			}
		}
		survivors = append(survivors, b)
	}

	if opts.MaxBytes > 0 {
		// Locked buckets and the shared module cache are real bytes on the disk
		// but cannot be evicted — the first belongs to a run in flight, the
		// second to every task at once. Under --concurrency they can fill the
		// ceiling on their own, and charging the pass with them makes it drain
		// every cold bucket and STILL finish over budget: nothing reclaimed, and
		// the next run recompiles from an empty GOCACHE, which is the one thing
		// this cache exists to prevent. So the pass only spends what eviction can
		// actually recover.
		var pinned, cold int64
		for _, b := range res.SkippedLive {
			pinned += b.Size
		}
		pinned += dirBytes(filepath.Join(root, toolcacheDirName, toolcacheShared))
		for _, b := range survivors {
			cold += b.Size
		}
		room := opts.MaxBytes - pinned
		// The unevictable share already fills the budget: evicting cannot bring
		// the total under it, so keep every warm bucket and let a later pass —
		// once those runs are done — do the real work.
		if room <= 0 {
			res.Kept = survivors
			return res, nil
		}
		// survivors is LRU-first (scan sorts by LastUsed ascending).
		var kept []ToolcacheBucket
		for _, b := range survivors {
			if cold > room {
				if remove(b) {
					cold -= b.Size
					continue
				}
			}
			kept = append(kept, b)
		}
		survivors = kept
	}
	res.Kept = survivors
	return res, nil
}
