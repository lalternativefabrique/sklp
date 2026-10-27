package space

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func mkBucket(t *testing.T, root, name string, size int, origin *ToolcacheOrigin) string {
	t.Helper()
	dir := filepath.Join(root, toolcacheDirName, name)
	if err := os.MkdirAll(filepath.Join(dir, "go"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go", "blob"), make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	if origin != nil {
		if err := writeToolcacheOrigin(dir, origin.Repo, origin.Task, origin.LastRun); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func removedNames(res ToolcacheGCResult) map[string]bool {
	out := map[string]bool{}
	for _, b := range res.Removed {
		out[b.Name] = true
	}
	return out
}

// A bucket whose recorded checkout is gone is reclaimable; one whose checkout
// still exists is not. This is the whole point of the origin sidecar — without
// it both look identical from the bucket name alone.
func TestCollectToolcache_RemovesOnlyDeadCheckouts(t *testing.T) {
	root := t.TempDir()
	liveRepo := filepath.Join(t.TempDir(), "live-worktree")
	if err := os.MkdirAll(liveRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	deadRepo := filepath.Join(t.TempDir(), "deleted-worktree")

	mkBucket(t, root, "ci-alive", 64, &ToolcacheOrigin{Repo: liveRepo, Task: "ci", LastRun: time.Now()})
	mkBucket(t, root, "ci-dead", 64, &ToolcacheOrigin{Repo: deadRepo, Task: "ci", LastRun: time.Now()})

	res, err := collectToolcacheIn(root, ToolcacheGCOpts{Orphans: true}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got := removedNames(res)
	if !got["ci-dead"] {
		t.Errorf("bucket of a deleted checkout was kept; removed=%v", got)
	}
	if got["ci-alive"] {
		t.Errorf("bucket of an existing checkout was removed; removed=%v", got)
	}
	if _, err := os.Stat(filepath.Join(root, toolcacheDirName, "ci-alive")); err != nil {
		t.Errorf("live bucket unlinked: %v", err)
	}
}

// Without a sidecar the collector cannot prove a bucket is dead, so it must
// keep it. Deleting on absence of evidence would wipe the cache of every
// pre-upgrade bucket on first run.
func TestCollectToolcache_KeepsBucketWithoutOrigin(t *testing.T) {
	root := t.TempDir()
	mkBucket(t, root, "ci-legacy", 64, nil)

	res, err := collectToolcacheIn(root, ToolcacheGCOpts{Orphans: true}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if removedNames(res)["ci-legacy"] {
		t.Error("bucket without an origin sidecar was removed on an orphan pass")
	}
}

// A locked bucket is bind-mounted into a running container: unlinking it under
// a live `go build` breaks that pipeline. This holds even when the bucket is
// otherwise the most reclaimable one there is (dead checkout, zero budget).
func TestCollectToolcache_SkipsLockedBucket(t *testing.T) {
	root := t.TempDir()
	deadRepo := filepath.Join(t.TempDir(), "gone")
	dir := mkBucket(t, root, "ci-running", 64, &ToolcacheOrigin{Repo: deadRepo, Task: "ci", LastRun: time.Now()})
	release := lockToolcacheBucket(dir)
	defer release()

	res, err := collectToolcacheIn(root, ToolcacheGCOpts{Orphans: true, MaxBytes: 1}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if removedNames(res)["ci-running"] {
		t.Fatal("locked bucket was removed while a run holds it mounted")
	}
	if len(res.SkippedLive) != 1 {
		t.Errorf("locked bucket not reported as skipped: %+v", res.SkippedLive)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("locked bucket unlinked: %v", err)
	}
}

// Releasing the lock makes the bucket collectable again — the run is over.
func TestCollectToolcache_CollectsAfterLockReleased(t *testing.T) {
	root := t.TempDir()
	deadRepo := filepath.Join(t.TempDir(), "gone")
	dir := mkBucket(t, root, "ci-finished", 64, &ToolcacheOrigin{Repo: deadRepo, Task: "ci", LastRun: time.Now()})
	lockToolcacheBucket(dir)()

	res, err := collectToolcacheIn(root, ToolcacheGCOpts{Orphans: true}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !removedNames(res)["ci-finished"] {
		t.Errorf("bucket left behind after its run released the lock; removed=%v", removedNames(res))
	}
}

// A run killed with SIGKILL never runs its release, leaving a lock naming a
// dead pid. Honouring it would pin the bucket forever — exactly the leak this
// collector exists to stop.
func TestCollectToolcache_IgnoresStaleLockOfDeadProcess(t *testing.T) {
	root := t.TempDir()
	deadRepo := filepath.Join(t.TempDir(), "gone")
	dir := mkBucket(t, root, "ci-killed", 64, &ToolcacheOrigin{Repo: deadRepo, Task: "ci", LastRun: time.Now()})
	// A pid that cannot be running: the kernel reserves 0 as "not a process".
	if err := os.WriteFile(filepath.Join(dir, toolcacheLockFn), []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := collectToolcacheIn(root, ToolcacheGCOpts{Orphans: true}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !removedNames(res)["ci-killed"] {
		t.Errorf("stale lock of a dead process pinned the bucket; removed=%v", removedNames(res))
	}
}

// Checkouts that all still exist are not orphans, so only the LRU pass bounds
// the total — evicting oldest-first until the budget is met.
func TestCollectToolcache_LRUEvictsOldestFirst(t *testing.T) {
	root := t.TempDir()
	repo := t.TempDir()
	old := &ToolcacheOrigin{Repo: repo, Task: "ci", LastRun: time.Now().Add(-72 * time.Hour)}
	mid := &ToolcacheOrigin{Repo: repo, Task: "ci", LastRun: time.Now().Add(-1 * time.Hour)}
	recent := &ToolcacheOrigin{Repo: repo, Task: "ci", LastRun: time.Now()}
	mkBucket(t, root, "ci-old", 4096, old)
	mkBucket(t, root, "ci-mid", 4096, mid)
	mkBucket(t, root, "ci-recent", 4096, recent)

	res, err := collectToolcacheIn(root, ToolcacheGCOpts{Orphans: true, MaxBytes: 8192}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got := removedNames(res)
	if !got["ci-old"] {
		t.Errorf("LRU kept the least-recently-used bucket; removed=%v", got)
	}
	if got["ci-recent"] {
		t.Errorf("LRU evicted the most-recently-used bucket; removed=%v", got)
	}
}

// The shared module cache is content-addressed and owned by no single
// checkout; reclaiming it is `sklp clean`'s job, not the
// collector's, and dropping it would force a full module re-download.
func TestCollectToolcache_NeverTouchesSharedBucket(t *testing.T) {
	root := t.TempDir()
	mkBucket(t, root, toolcacheShared, 4096, nil)

	res, err := collectToolcacheIn(root, ToolcacheGCOpts{Orphans: true, MaxBytes: 1}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if removedNames(res)[toolcacheShared] {
		t.Fatal("shared module cache was reclaimed by the collector")
	}
	if _, err := os.Stat(filepath.Join(root, toolcacheDirName, toolcacheShared)); err != nil {
		t.Fatalf("shared bucket unlinked: %v", err)
	}
}

func TestCollectToolcache_DryRunReportsWithoutUnlinking(t *testing.T) {
	root := t.TempDir()
	deadRepo := filepath.Join(t.TempDir(), "gone")
	dir := mkBucket(t, root, "ci-dead", 64, &ToolcacheOrigin{Repo: deadRepo, Task: "ci", LastRun: time.Now()})

	res, err := collectToolcacheIn(root, ToolcacheGCOpts{Orphans: true, DryRun: true}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !removedNames(res)["ci-dead"] {
		t.Error("dry run did not report the orphan it would remove")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("dry run unlinked the bucket: %v", err)
	}
}

// The sidecar must survive a round-trip, since the collector's whole liveness
// decision reads back what the run wrote.
func TestToolcacheOrigin_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	when := time.Now().Truncate(time.Second)
	if err := writeToolcacheOrigin(dir, dir, "ci-transcript", when); err != nil {
		t.Fatal(err)
	}
	got, ok := readToolcacheOrigin(dir)
	if !ok {
		t.Fatal("origin written but not readable back")
	}
	if got.Repo != dir || got.Task != "ci-transcript" {
		t.Errorf("origin round-trip mismatch: %+v", got)
	}
	if !got.LastRun.Equal(when) {
		t.Errorf("LastRun = %v, want %v", got.LastRun, when)
	}
}

// Space.Close keeps per-space state on disk by design (a deployment needs it),
// which for CI means one orphan directory per task × checkout, forever. The CI
// path must drop its own.
func TestRemoveCISpaceState_DropsFinishedSpaceState(t *testing.T) {
	root := t.TempDir()
	s := &Space{cacheRoot: root, scope: newScope("ci-abcd1234", root)}
	dir := s.scope.spaceDir()
	for _, sub := range []string{"overlay", "runc-state", "volumes"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	removeCISpaceState(s)

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("CI space state survived the run: stat err = %v", err)
	}
}

// The standalone (unnamed) space uses the legacy flat layout, where spaceDir
// resolves to the shared spaces/ root — removing it would wipe every other
// space's state.
func TestRemoveCISpaceState_LeavesStandaloneSpaceAlone(t *testing.T) {
	root := t.TempDir()
	other := filepath.Join(spacesRoot(root), "deploy-prod")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	s := &Space{cacheRoot: root, scope: newScope("", root)}

	removeCISpaceState(s)

	if _, err := os.Stat(other); err != nil {
		t.Fatalf("standalone cleanup wiped another space's state: %v", err)
	}
}

func TestToolcacheBudget_EnvOverride(t *testing.T) {
	t.Setenv("SKLP_TOOLCACHE_MAX_BYTES", "4096")
	if got := toolcacheBudget(); got != 4096 {
		t.Errorf("budget = %d, want 4096", got)
	}
	t.Setenv("SKLP_TOOLCACHE_MAX_BYTES", "0")
	if got := toolcacheBudget(); got != 0 {
		t.Errorf("budget = %d, want 0 (LRU disabled)", got)
	}
	t.Setenv("SKLP_TOOLCACHE_MAX_BYTES", "not-a-number")
	if got := toolcacheBudget(); got != defaultToolcacheBudget {
		t.Errorf("malformed budget = %d, want default %d", got, defaultToolcacheBudget)
	}
}

// Under --concurrency the in-flight runs' buckets are locked, so they cannot be
// evicted. Charging them against the ceiling makes the pass drain every cold
// bucket it CAN remove and still finish over budget: the reclaim frees nothing
// and the next run recompiles from an empty GOCACHE — the cache exists to
// prevent exactly that.
func TestCollectToolcache_LiveBucketsOverBudgetDoNotDrainColdOnes(t *testing.T) {
	root := t.TempDir()
	liveRepo := t.TempDir()
	for _, n := range []string{"ci-live1", "ci-live2"} {
		dir := mkBucket(t, root, n, 8192, &ToolcacheOrigin{Repo: liveRepo, Task: "ci", LastRun: time.Now()})
		if err := os.WriteFile(filepath.Join(dir, toolcacheLockFn), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mkBucket(t, root, "ci-warm", 4096, &ToolcacheOrigin{Repo: liveRepo, Task: "ci", LastRun: time.Now().Add(-time.Hour)})

	res, err := collectToolcacheIn(root, ToolcacheGCOpts{MaxBytes: 12288}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if removedNames(res)["ci-warm"] {
		t.Error("evicted a reusable GOCACHE to chase a budget the locked buckets already blow; " +
			"the next run recompiles from scratch and nothing was gained")
	}
}
