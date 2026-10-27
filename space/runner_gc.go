package space

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/lalternativefabrique/sklp/space/runnerbuild"
)

const runnerDirName = "runner"

// defaultRunnerBudget bounds the total size of built runner rootfs trees.
// Measured at 1.7-2.5 GiB each for this repo's own space.yaml (a full base copy
// plus the recipe's apt packages and Go tools). The cache key folds in the
// baked sklp binary, so every CLI upgrade on a node strands the entire previous
// set: without a ceiling the tree grows without bound and only a manual
// `sklp clean` — which also destroys the rootfs in use, forcing a full rebuild
// on the next run — ever reclaims it. 8 GiB holds the live entry plus two or
// three recent ones, so a rollback or a branch on an older space.yaml still
// hits warm, while a stale generation cannot outlive a few runs.
const defaultRunnerBudget int64 = 8 << 30

// DefaultRunnerBudget exposes the resolved ceiling so the CLI's default matches
// what an automatic end-of-run collection uses.
func DefaultRunnerBudget() int64 { return runnerBudget() }

// runnerBudget resolves the LRU ceiling. SKLP_RUNNER_MAX_BYTES overrides the
// default; 0 disables the pass entirely.
func runnerBudget() int64 {
	raw := os.Getenv("SKLP_RUNNER_MAX_BYTES")
	if raw == "" {
		return defaultRunnerBudget
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return defaultRunnerBudget
	}
	return n
}

// RunnerEntry is one runner/<hash> directory as seen by the collector.
type RunnerEntry struct {
	Hash string
	Path string
	Size int64
	// Built is true when the .built sentinel is present. An entry without it is
	// the debris of a build that died between MkdirAll and the sentinel write:
	// unusable by definition, so it is reclaimed regardless of budget.
	Built bool
	// Live is true when a run still holds the entry (pid lock) or the kernel
	// still has it mounted — the latter covers a persistent space, whose
	// containers outlive the process that built the rootfs.
	Live bool
	// LastUsed is the recorded usage stamp, falling back to the directory mtime
	// for entries built before the sidecar existed.
	LastUsed time.Time
}

// RunnerGCOpts selects what a collection pass reclaims.
type RunnerGCOpts struct {
	// Partial removes entries with no .built sentinel.
	Partial bool
	// MaxBytes, when > 0, evicts least-recently-used entries until the total
	// falls at or below it. Applied after partial removal.
	MaxBytes int64
	// DryRun reports what would be removed without unlinking anything.
	DryRun bool
}

// RunnerGCResult reports a collection pass.
type RunnerGCResult struct {
	Removed      []RunnerEntry
	Kept         []RunnerEntry
	SkippedLive  []RunnerEntry
	ReclaimedFor int64
	Errors       map[string]error
}

func scanRunnerEntries(root string) ([]RunnerEntry, error) {
	dir := filepath.Join(root, runnerDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	mounts := mountedRunnerPaths()
	var out []RunnerEntry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		r := RunnerEntry{Hash: e.Name(), Path: filepath.Join(dir, e.Name())}
		r.Live = runnerbuild.IsLocked(r.Path) || runnerEntryMounted(mounts, r.Path)
		_, serr := os.Stat(filepath.Join(r.Path, ".built"))
		r.Built = serr == nil
		if u, ok := runnerbuild.ReadUsage(r.Path); ok {
			r.LastUsed, r.Size = u.LastUsed, u.Size
		}
		// Only entries predating the sidecar (or a partial build, which records
		// no size) pay the walk. The collector runs after every pipeline, and a
		// rootfs is ~75k inodes: measuring each one every time would tax every CI
		// run for a figure that cannot change once .built is written.
		if r.Size == 0 {
			r.Size = dirBytes(r.Path)
		}
		if r.LastUsed.IsZero() {
			if fi, ferr := e.Info(); ferr == nil {
				r.LastUsed = fi.ModTime()
			}
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastUsed.Before(out[j].LastUsed) })
	return out, nil
}

// CollectRunner reclaims built runner rootfs trees under <cacheRoot>/runner.
// Entries a live run holds are never touched: the rootfs is bind-mounted into
// that run's containers, and unlinking it leaves the mount pointing at a
// vanished source — the container's own binaries disappear mid-step.
func CollectRunner(opts RunnerGCOpts) (RunnerGCResult, error) {
	root, err := cacheRoot()
	if err != nil {
		return RunnerGCResult{}, err
	}
	return collectRunnerIn(root, opts)
}

func collectRunnerIn(root string, opts RunnerGCOpts) (RunnerGCResult, error) {
	res := RunnerGCResult{Errors: map[string]error{}}
	entries, err := scanRunnerEntries(root)
	if err != nil {
		return res, fmt.Errorf("scan %s: %w", runnerDirName, err)
	}

	remove := func(r RunnerEntry) bool {
		if opts.DryRun {
			res.Removed = append(res.Removed, r)
			res.ReclaimedFor += r.Size
			return true
		}
		// The build steps run as root in a userns and chown files to mapped
		// subuids, which the host uid cannot unlink directly.
		if rerr := subidRemoveAll(r.Path); rerr != nil {
			res.Errors[r.Hash] = rerr
			return false
		}
		res.Removed = append(res.Removed, r)
		res.ReclaimedFor += r.Size
		return true
	}

	var survivors []RunnerEntry
	for _, r := range entries {
		if r.Live {
			res.SkippedLive = append(res.SkippedLive, r)
			continue
		}
		if opts.Partial && !r.Built {
			if remove(r) {
				continue
			}
		}
		survivors = append(survivors, r)
	}

	if opts.MaxBytes > 0 {
		// Pinned entries count against the ceiling — they are real bytes on the
		// disk — but they cannot be evicted. Under --concurrency the live runs
		// alone can exceed the budget, and then evicting every cold entry still
		// finishes over it: the reclaim buys nothing and the next run pays a full
		// rebuild (apt + five `go install`s) for the entry it would have hit warm.
		// So the pass only spends what eviction can actually recover, and stops
		// once the evictable share fits.
		var live, cold int64
		for _, r := range res.SkippedLive {
			live += r.Size
		}
		for _, r := range survivors {
			cold += r.Size
		}
		room := opts.MaxBytes - live
		// Live entries alone already blow the ceiling: eviction cannot bring the
		// total under it, so it would be pure cost. Keep every cold entry warm
		// and let the next pass — when those pins are gone — do the real work.
		if room <= 0 {
			res.Kept = survivors
			return res, nil
		}
		// survivors is LRU-first (scan sorts by LastUsed ascending).
		var kept []RunnerEntry
		for _, r := range survivors {
			if cold > room {
				if remove(r) {
					cold -= r.Size
					continue
				}
			}
			kept = append(kept, r)
		}
		survivors = kept
	}
	res.Kept = survivors
	return res, nil
}

// collectRunnerAfterRun is the end-of-run reclaim hook, the counterpart of
// collectToolcacheAfterRun. It runs for passing and failing pipelines alike and
// reports only when it freed something, so a normal run stays silent. Errors
// are advisory: an entry the collector cannot unlink must never turn a green
// pipeline red.
func collectRunnerAfterRun(out io.Writer) {
	res, err := CollectRunner(RunnerGCOpts{Partial: true, MaxBytes: runnerBudget()})
	if err != nil || out == nil || len(res.Removed) == 0 {
		return
	}
	fmt.Fprintf(out, "runner cache: reclaimed %s from %d stale rootfs(es)\n",
		humanToolcacheBytes(res.ReclaimedFor), len(res.Removed))
}
