package space

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// liveDigests is the set of image digests some persisted space manifest still
// names, resolved through the refs/ pointers the pull path writes.
//
// Manifests are read from disk (~/.config/sklp/spaces/*/manifest.json) rather
// than asked of the running supervisors: a supervisor whose socket is briefly
// unresponsive must not make its images collectable. Reading the file also
// keeps this package free of a control import, which would be a cycle.
func liveDigests(cacheRoot string) (map[string]bool, error) {
	live := map[string]bool{}
	dir := filepath.Join(configRootDir(), "sklp", "spaces")
	spaces, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return live, nil
		}
		return nil, err
	}
	for _, s := range spaces {
		if !s.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, s.Name(), "manifest.json"))
		if err != nil {
			continue
		}
		var spec struct {
			Services []struct {
				Image string `json:"image"`
			} `json:"services"`
		}
		if err := json.Unmarshal(b, &spec); err != nil {
			continue
		}
		for _, svc := range spec.Services {
			if svc.Image == "" {
				continue
			}
			if digest := resolveRefDigest(cacheRoot, svc.Image); digest != "" {
				live[digest] = true
			}
		}
	}
	return live, nil
}

// resolveRefDigest maps a manifest image reference to its extracted digest. A
// digest-pinned ref carries it already; a tag goes through the refs/ pointer
// the pull recorded. An unresolvable ref yields "" — it protects nothing, which
// is safe: an image no ref points at is exactly what this collector removes.
func resolveRefDigest(cacheRoot, ref string) string {
	if _, digest, ok := strings.Cut(ref, "@sha256:"); ok {
		return digest
	}
	normalized, err := normalizeRef(ref)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(readCachedDigest(cacheRoot, normalized, runtime.GOARCH), "sha256:")
}

// mountedLowerDigests is the set of digests currently held as an overlay
// lowerdir by some process on this host.
//
// This is the guard that carries the whole collector: a roll in flight has two
// legitimate generations — the retiring container still running on the old
// lower, the new one starting — and only the new one is in the manifest. The
// kernel's own mount table is the authority on what is in use; a manifest is
// only a statement of intent.
//
// Every /proc/<pid>/mountinfo is scanned, not just our own: sklp holds each
// service overlay in the mount namespace of a per-service holder process, so
// the mounts are invisible from this process's table. Unreadable entries (other
// users, races with exiting processes) are skipped; missing a mount can only
// make the collector keep a digest it could have removed on the next pass.
func mountedLowerDigests(rootfsDir string) map[string]bool {
	mounted := map[string]bool{}
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return mounted
	}
	spacesDir := spacesRoot(filepath.Dir(rootfsDir))
	for _, p := range procs {
		if _, err := strconv.Atoi(p.Name()); err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", p.Name(), "mountinfo"))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			opts, ok := cutMountOption(line, "lowerdir=")
			if !ok {
				continue
			}
			for _, lower := range strings.Split(opts, ":") {
				if digest := lowerDigest(lower, rootfsDir, spacesDir); digest != "" {
					mounted[digest] = true
				}
			}
		}
	}
	return mounted
}

// cutMountOption extracts one comma-separated mount option's value from a
// mountinfo line.
// lowerDigest is the image digest an overlay lower path refers to: a digest
// dir of the shared cache, or a space's own copy of one (see childLowerFor);
// "" for a path that is neither.
func lowerDigest(lower, rootfsDir, spacesDir string) string {
	sep := string(os.PathSeparator)
	if rest, ok := strings.CutPrefix(lower, rootfsDir+sep); ok {
		digest, _, _ := strings.Cut(rest, sep)
		return digest
	}
	if rest, ok := strings.CutPrefix(lower, spacesDir+sep); ok {
		parts := strings.Split(rest, sep)
		if len(parts) >= 3 && parts[1] == "rootfs" {
			return parts[2]
		}
	}
	return ""
}

// spaceCopiesOf lists every space's own copy of digest.
func spaceCopiesOf(root, digest string) []string {
	matches, _ := filepath.Glob(filepath.Join(spacesRoot(root), "*", "rootfs", digest))
	return matches
}

// orphanSpaceCopies lists space copies whose digest is no longer in the
// shared cache: the shared rootfs went, the copy must follow.
func orphanSpaceCopies(root string, shared map[string]bool) []string {
	var out []string
	matches, _ := filepath.Glob(filepath.Join(spacesRoot(root), "*", "rootfs", "*"))
	for _, m := range matches {
		if d := filepath.Base(m); !shared[d] && !strings.HasSuffix(d, ".copying") {
			out = append(out, m)
		}
	}
	return out
}

func cutMountOption(line, key string) (string, bool) {
	_, after, ok := strings.Cut(line, key)
	if !ok {
		return "", false
	}
	value, _, _ := strings.Cut(after, ",")
	return value, true
}

// configRootDir resolves the XDG config base, mirroring control.configRoot —
// duplicated rather than imported because control imports this package.
func configRootDir() string {
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		return base
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "sklp-config")
	}
	return filepath.Join(home, ".config")
}

// imageGCKeepPerRepo is how many generations of one image repository the
// collector keeps beyond the ones in use. It bounds the store on a service
// that rolls faster than the grace window: at N rolls a day the grace alone
// settles at N resident generations, which is what filled the disk.
//
// Three is the smallest count that still buys what the grace period bought —
// the generation now running, the one it replaced, and one before that — so a
// bad roll can be restarted onto its predecessor without a re-pull.
const imageGCKeepPerRepo = 3

// ImageGCKeepPerRepo exposes the retention count so a caller can report it
// without restating the number.
func ImageGCKeepPerRepo() int { return imageGCKeepPerRepo }

// generation is one extracted rootfs, tied to the repository it belongs to so
// retention can be counted per service rather than across the whole node. The
// store is machine-global and keyed by digest alone, so without this grouping
// "keep 3" on a node running seven services would evict four of them.
type generation struct {
	Digest   string
	Repo     string
	Modified time.Time
}

// repoByDigest maps each extracted digest to the repository its refs/ pointers
// name. A digest no pointer resolves to — or whose pointers predate the
// repository being recorded — maps to "", and an ungrouped digest is left to
// the grace window alone rather than counted against another service's quota.
func repoByDigest(root string) map[string]string {
	out := map[string]string{}
	dir := filepath.Join(root, refCacheDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		digest, repo := readCachedPointer(root, e.Name())
		if digest == "" || repo == "" {
			continue
		}
		out[digest] = repo
	}
	return out
}

// keepNewestPerRepo returns the digests to retain purely on recency: the
// newest imageGCKeepPerRepo of each repository. Callers apply it on top of the
// live and mounted sets, which pin regardless of age — this only decides which
// of the RETIRED generations are worth a rollback.
//
// Ungrouped generations (no repository recorded) are all retained here and
// left to the grace window, since counting them together would let one
// service's rolls evict another's spare.
func keepNewestPerRepo(gens []generation, keep int) map[string]bool {
	byRepo := map[string][]generation{}
	out := map[string]bool{}
	for _, g := range gens {
		if g.Repo == "" {
			out[g.Digest] = true
			continue
		}
		byRepo[g.Repo] = append(byRepo[g.Repo], g)
	}
	for _, list := range byRepo {
		sort.Slice(list, func(i, j int) bool {
			if !list[i].Modified.Equal(list[j].Modified) {
				return list[i].Modified.After(list[j].Modified)
			}
			return list[i].Digest < list[j].Digest
		})
		for i, g := range list {
			if i >= keep {
				break
			}
			out[g.Digest] = true
		}
	}
	return out
}
