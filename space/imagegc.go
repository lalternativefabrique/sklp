package space

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ImageGCResult reports what an ImageGC pass removed, so a caller can log or
// print it without re-walking the cache.
type ImageGCResult struct {
	Removed     []RemovedImage
	FreedBytes  int64
	KeptLive    int
	KeptRecent  int
	KeptMounted int
	// EvictedOverKeep counts generations removed for exceeding the per-repo
	// retention rather than for age, which is the difference between a node
	// that rolls slowly and one whose rolls outpace the grace window.
	EvictedOverKeep int
}

// RemovedImage is one collected generation: its digest and the space it freed.
type RemovedImage struct {
	Digest string
	Size   int64
	// Reason says which rule collected it, so a report can tell an image
	// nothing referenced from one retired past the per-service retention.
	Reason string
}

// imageGCGrace is how long an unreferenced rootfs is kept after it stops being
// referenced. It buys a restart without a re-pull when a freshly rolled tag
// turns out to be broken at boot — NOT a rollback: generations share their
// volumes, so on an app with migrations, going back an image leaves the schema
// ahead of the code.
const imageGCGrace = 24 * time.Hour

// ImageGC removes extracted image rootfs directories that nothing references
// any more, with their config and ref pointers.
//
// Deployments roll by tag, and every distinct tag extracts a fresh
// rootfs/<digest> of its own. Nothing ever removed the previous one, so a node
// accumulated one full image tree per generation ever deployed (71 of a single
// service, filling a 96G disk). This is the missing counterpart of the pull
// path.
//
// A digest survives when ANY of:
//   - live: some space manifest names an image resolving to it;
//   - mounted: some process holds it as an overlay lowerdir — a roll in flight
//     has two legitimate generations and only the new one is in the manifest,
//     so the manifest alone would delete the running container's lower;
//   - recent: it was modified within imageGCGrace AND is among the newest
//     imageGCKeepPerRepo generations of its repository. The grace alone was
//     unbounded in the one case that matters — a service rolling faster than
//     the window keeps every generation of the day — so recency buys a spare
//     for a rollback, not an archive.
//
// Removal takes rootfs/<digest>, configs/<digest>.json and every refs/ pointer
// aimed at it together. Dropping only the rootfs leaves a ref pointing at a
// vanished lower, and the next deploy of that tag starts a container on a
// near-empty merged dir — surfacing far from here as "entrypoint not found in
// $PATH".
func ImageGC(dryRun bool) (ImageGCResult, error) {
	root, err := cacheRoot()
	if err != nil {
		return ImageGCResult{}, err
	}
	return imageGCAt(root, dryRun, time.Now())
}

func imageGCAt(root string, dryRun bool, now time.Time) (ImageGCResult, error) {
	var res ImageGCResult

	rootfsDir := filepath.Join(root, "rootfs")
	entries, err := os.ReadDir(rootfsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return res, nil
		}
		return res, fmt.Errorf("read image cache: %w", err)
	}

	live, err := liveDigests(root)
	if err != nil {
		return res, err
	}
	mounted := mountedLowerDigests(rootfsDir)
	refs := refsByDigest(root)

	shared := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() && !strings.HasSuffix(e.Name(), ".staging") {
			shared[e.Name()] = true
		}
	}
	for _, copy := range orphanSpaceCopies(root, shared) {
		digest := filepath.Base(copy)
		if mounted[digest] {
			continue
		}
		size := dirSize(copy)
		if !dryRun {
			if err := subidRemoveAll(copy); err != nil {
				return res, fmt.Errorf("remove space copy %s: %w", copy, err)
			}
		}
		res.Removed = append(res.Removed, RemovedImage{Digest: digest, Size: size, Reason: "space copy of an image no longer cached"})
		res.FreedBytes += size
	}

	repos := repoByDigest(root)
	gens := make([]generation, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || strings.HasSuffix(e.Name(), ".staging") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		gens = append(gens, generation{Digest: e.Name(), Repo: repos[e.Name()], Modified: info.ModTime()})
	}
	spare := keepNewestPerRepo(gens, imageGCKeepPerRepo)

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		digest := e.Name()
		// Staging dirs belong to an in-flight extraction, not to a generation.
		if strings.HasSuffix(digest, ".staging") {
			continue
		}
		switch {
		case live[digest]:
			res.KeptLive++
			continue
		case mounted[digest]:
			res.KeptMounted++
			continue
		}
		dir := filepath.Join(rootfsDir, digest)
		info, err := e.Info()
		overKeep := !spare[digest]
		if err == nil && !overKeep && now.Sub(info.ModTime()) < imageGCGrace {
			res.KeptRecent++
			continue
		}

		reason := "no space references it"
		if overKeep {
			res.EvictedOverKeep++
			reason = fmt.Sprintf("past the %d newest of its service", imageGCKeepPerRepo)
		}
		size := dirSize(dir)
		copies := spaceCopiesOf(root, digest)
		for _, c := range copies {
			size += dirSize(c)
		}
		if !dryRun {
			for _, c := range copies {
				if err := subidRemoveAll(c); err != nil {
					return res, fmt.Errorf("remove space copy %s: %w", c, err)
				}
			}
			if err := subidRemoveAll(dir); err != nil {
				return res, fmt.Errorf("remove rootfs %s: %w", digest, err)
			}
			if err := os.Remove(filepath.Join(root, "configs", digest+".json")); err != nil && !os.IsNotExist(err) {
				return res, fmt.Errorf("remove config %s: %w", digest, err)
			}
			for _, ref := range refs[digest] {
				if err := os.Remove(filepath.Join(root, refCacheDir, ref)); err != nil && !os.IsNotExist(err) {
					return res, fmt.Errorf("remove ref %s: %w", ref, err)
				}
			}
		}
		res.Removed = append(res.Removed, RemovedImage{Digest: digest, Size: size, Reason: reason})
		res.FreedBytes += size
	}

	sort.Slice(res.Removed, func(i, j int) bool { return res.Removed[i].Digest < res.Removed[j].Digest })
	return res, nil
}

// dirSize sums the apparent size of a tree, best-effort: an unreadable subtree
// only makes the reported total low, which never changes what gets removed.
func dirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // unreadable subtree: skip, do not abort
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// refsByDigest inverts the refs/ pointers so a digest's ref files can be
// removed with it.
func refsByDigest(root string) map[string][]string {
	out := map[string][]string{}
	dir := filepath.Join(root, refCacheDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		digest, _ := readCachedPointer(root, e.Name())
		if digest == "" {
			continue
		}
		out[digest] = append(out[digest], e.Name())
	}
	return out
}
