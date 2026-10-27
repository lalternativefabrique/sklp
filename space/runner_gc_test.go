package space

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func mkRunnerEntry(t *testing.T, root, hash string, size int, built bool, lastUsed time.Time) string {
	t.Helper()
	dir := filepath.Join(root, runnerDirName, hash)
	if err := os.MkdirAll(filepath.Join(dir, "rootfs", "usr"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rootfs", "usr", "blob"), make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	if built {
		if err := os.WriteFile(filepath.Join(dir, ".built"), []byte(hash+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if !lastUsed.IsZero() {
		blob, err := json.Marshal(map[string]any{"hash": hash, "lastUsed": lastUsed, "size": size})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".usage"), blob, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// freePid returns a pid no process holds. A hardcoded huge number will not do:
// past /proc/sys/kernel/pid_max the kernel answers EINVAL, not ESRCH, which is
// not the "process is gone" case under test.
func freePid(t *testing.T) int {
	t.Helper()
	for pid := 2; pid < 1<<22; pid++ {
		if pid != os.Getpid() && !processAlive(pid) {
			return pid
		}
	}
	t.Skip("no free pid on this machine")
	return 0
}

func removedHashes(res RunnerGCResult) map[string]bool {
	out := map[string]bool{}
	for _, r := range res.Removed {
		out[r.Hash] = true
	}
	return out
}

// The LRU pass must evict oldest-first and stop as soon as the total fits, not
// drain the cache: the entry a pipeline just used is the one whose rebuild
// costs the most (a full apt + go install pass), so an over-eager collector
// makes every run slower than no collector at all.
func TestCollectRunner_EvictsOldestUntilUnderBudget(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	mkRunnerEntry(t, root, "oldest", 4096, true, now.Add(-72*time.Hour))
	mkRunnerEntry(t, root, "middle", 4096, true, now.Add(-24*time.Hour))
	mkRunnerEntry(t, root, "newest", 4096, true, now)

	res, err := collectRunnerIn(root, RunnerGCOpts{MaxBytes: 9000})
	if err != nil {
		t.Fatal(err)
	}
	got := removedHashes(res)
	if !got["oldest"] {
		t.Errorf("least-recently-used entry was kept; removed=%v", got)
	}
	if got["newest"] {
		t.Errorf("most-recently-used entry was evicted; removed=%v", got)
	}
	if len(res.Removed) != 1 {
		t.Errorf("evicted %d entries, want 1 (stop as soon as the total fits)", len(res.Removed))
	}
	if _, err := os.Stat(filepath.Join(root, runnerDirName, "newest")); err != nil {
		t.Errorf("newest entry unlinked: %v", err)
	}
}

// A budget of 0 disables the LRU pass entirely, matching SKLP_RUNNER_MAX_BYTES=0.
func TestCollectRunner_ZeroBudgetEvictsNothing(t *testing.T) {
	root := t.TempDir()
	mkRunnerEntry(t, root, "a", 4096, true, time.Now().Add(-72*time.Hour))
	mkRunnerEntry(t, root, "b", 4096, true, time.Now())

	res, err := collectRunnerIn(root, RunnerGCOpts{MaxBytes: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 0 {
		t.Errorf("removed %d entries with the LRU pass disabled", len(res.Removed))
	}
}

// A build killed between MkdirAll and the sentinel write leaves a rootfs no
// cache hit will ever accept. It is dead weight by definition, so it goes
// regardless of the budget.
func TestCollectRunner_RemovesPartialBuilds(t *testing.T) {
	root := t.TempDir()
	mkRunnerEntry(t, root, "partial", 4096, false, time.Now())
	mkRunnerEntry(t, root, "complete", 4096, true, time.Now())

	res, err := collectRunnerIn(root, RunnerGCOpts{Partial: true})
	if err != nil {
		t.Fatal(err)
	}
	got := removedHashes(res)
	if !got["partial"] {
		t.Errorf("entry with no .built sentinel was kept; removed=%v", got)
	}
	if got["complete"] {
		t.Errorf("completed build was removed; removed=%v", got)
	}
}

// An entry pinned by a live pid is never reclaimed, even when the budget says
// it should be: its rootfs is mounted into a running container, and unlinking
// it makes the container's own binaries vanish mid-step.
func TestCollectRunner_KeepsLockedEntryOverBudget(t *testing.T) {
	root := t.TempDir()
	dir := mkRunnerEntry(t, root, "inuse", 8192, true, time.Now().Add(-72*time.Hour))
	if err := os.WriteFile(filepath.Join(dir, ".lock"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mkRunnerEntry(t, root, "cold", 8192, true, time.Now())

	res, err := collectRunnerIn(root, RunnerGCOpts{MaxBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	if removedHashes(res)["inuse"] {
		t.Fatal("entry held by a live pid was reclaimed")
	}
	if len(res.SkippedLive) != 1 {
		t.Errorf("SkippedLive=%d, want 1", len(res.SkippedLive))
	}
}

// A lock naming a pid that no longer exists is stale — a run killed with
// SIGKILL never runs its release. It must not pin a multi-GiB tree forever.
func TestCollectRunner_StaleLockDoesNotPin(t *testing.T) {
	root := t.TempDir()
	dir := mkRunnerEntry(t, root, "stale", 8192, true, time.Now().Add(-72*time.Hour))
	if err := os.WriteFile(filepath.Join(dir, ".lock"), []byte(strconv.Itoa(freePid(t))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := collectRunnerIn(root, RunnerGCOpts{MaxBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !removedHashes(res)["stale"] {
		t.Errorf("entry held only by a dead pid was kept; removed=%v", removedHashes(res))
	}
}

// DryRun reports the same decision without unlinking anything.
func TestCollectRunner_DryRunKeepsDisk(t *testing.T) {
	root := t.TempDir()
	mkRunnerEntry(t, root, "old", 4096, true, time.Now().Add(-72*time.Hour))
	mkRunnerEntry(t, root, "new", 4096, true, time.Now())

	res, err := collectRunnerIn(root, RunnerGCOpts{MaxBytes: 4096, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) == 0 {
		t.Fatal("dry run reported nothing to reclaim")
	}
	for _, r := range res.Removed {
		if _, err := os.Stat(r.Path); err != nil {
			t.Errorf("dry run unlinked %s: %v", r.Hash, err)
		}
	}
}

// An entry predating the .usage sidecar falls back to the directory mtime, so
// it still takes a position in the LRU order rather than sorting as the zero
// time and being evicted first on every pass.
func TestCollectRunner_FallsBackToMtimeWithoutSidecar(t *testing.T) {
	root := t.TempDir()
	legacy := mkRunnerEntry(t, root, "legacy", 4096, true, time.Time{})
	old := time.Now().Add(-96 * time.Hour)
	if err := os.Chtimes(legacy, old, old); err != nil {
		t.Fatal(err)
	}
	mkRunnerEntry(t, root, "recent", 4096, true, time.Now())

	entries, err := scanRunnerEntries(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("scanned %d entries, want 2", len(entries))
	}
	if entries[0].Hash != "legacy" {
		t.Errorf("LRU order = %s first, want legacy (mtime fallback)", entries[0].Hash)
	}
	if entries[0].LastUsed.IsZero() {
		t.Error("legacy entry has a zero LastUsed; mtime fallback did not apply")
	}
}

// A hash that is a prefix of another must not borrow its liveness: a bare
// substring match over the mount table would report the wrong entry live and
// keep a dead tree forever.
func TestRunnerEntryMounted_PrefixIsNotAMatch(t *testing.T) {
	lines := []string{
		"420 61 0:52 / /run/x rw - overlay overlay rw,lowerdir=/home/u/.cache/sklp/runner/abcdef12/rootfs",
	}
	if !runnerEntryMounted(lines, "/home/u/.cache/sklp/runner/abcdef12") {
		t.Error("mounted entry reported free")
	}
	if runnerEntryMounted(lines, "/home/u/.cache/sklp/runner/abcdef") {
		t.Error("a hash that is only a prefix of a mounted one was reported live")
	}
}

// Under --concurrency the live runs alone can exceed the budget. Their entries
// are pinned and cannot be evicted, so charging them against the ceiling makes
// the pass drain every cold entry it CAN remove and still finish over budget —
// the eviction is pure loss, and the next run pays a full rebuild (apt + five
// `go install`s, minutes) for a reclaim that freed nothing.
func TestCollectRunner_LiveEntriesOverBudgetDoNotDrainColdOnes(t *testing.T) {
	root := t.TempDir()
	for _, h := range []string{"live1", "live2"} {
		dir := mkRunnerEntry(t, root, h, 8192, true, time.Now())
		if err := os.WriteFile(filepath.Join(dir, ".lock"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The entry the next run would hit warm.
	mkRunnerEntry(t, root, "reusable", 4096, true, time.Now().Add(-time.Hour))

	res, err := collectRunnerIn(root, RunnerGCOpts{MaxBytes: 12288})
	if err != nil {
		t.Fatal(err)
	}
	if removedHashes(res)["reusable"] {
		t.Error("evicted a reusable entry to chase a budget the pinned live entries already blow; " +
			"the next run rebuilds from scratch and nothing was gained")
	}
}

// The collector runs after every pipeline, so it must read the size recorded at
// build time rather than walking ~75k inodes per entry. A sidecar that carries
// one is authoritative: no walk, and the figure it reports is that size.
func TestScanRunnerEntries_UsesRecordedSizeWithoutWalking(t *testing.T) {
	root := t.TempDir()
	dir := mkRunnerEntry(t, root, "sized", 4096, true, time.Now())
	// A file added behind the sidecar's back: a walk would count it, the
	// recorded size would not. Distinguishes the two paths.
	if err := os.WriteFile(filepath.Join(dir, "rootfs", "extra"), make([]byte, 9999), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := scanRunnerEntries(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("scanned %d entries, want 1", len(entries))
	}
	if entries[0].Size != 4096 {
		t.Errorf("Size = %d, want 4096 (the recorded figure, not a fresh walk)", entries[0].Size)
	}
}

// An entry predating the sidecar has no recorded size, so it must still be
// measured — otherwise it would weigh 0 and never be evicted, and a cache full
// of legacy entries would grow unbounded exactly as before.
func TestScanRunnerEntries_WalksEntriesWithoutRecordedSize(t *testing.T) {
	root := t.TempDir()
	mkRunnerEntry(t, root, "legacy", 4096, true, time.Time{})

	entries, err := scanRunnerEntries(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("scanned %d entries, want 1", len(entries))
	}
	if entries[0].Size < 4096 {
		t.Errorf("Size = %d, want the measured tree (>= 4096); a legacy entry must not weigh 0", entries[0].Size)
	}
}
