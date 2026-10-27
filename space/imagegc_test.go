package space

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// gcFixture lays out a cache with one extracted rootfs per digest, a refs
// pointer per tag, and a manifest naming the images a space wants. Returns the
// cache root the collector operates on.
func gcFixture(t *testing.T, spaces map[string][]string, refs map[string]string, digests []string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	root := filepath.Join(home, "cache", "sklp", "embedded-runtime")

	for _, d := range digests {
		mustMkFile(t, filepath.Join(root, "rootfs", d, ".extracted"))
		mustMkFile(t, filepath.Join(root, "rootfs", d, "bin", "sh"))
		mustMkFile(t, filepath.Join(root, "configs", d+".json"))
	}
	for ref, digest := range refs {
		normalized, err := normalizeRef(ref)
		if err != nil {
			t.Fatalf("normalizeRef(%q): %v", ref, err)
		}
		writeCachedDigest(root, normalized, runtime.GOARCH, digest)
	}
	for space, images := range spaces {
		spec := `{"services":[`
		for i, img := range images {
			if i > 0 {
				spec += ","
			}
			spec += `{"name":"s` + string(rune('a'+i)) + `","image":"` + img + `"}`
		}
		spec += `]}`
		path := filepath.Join(home, "config", "sklp", "spaces", space, "manifest.json")
		mustMkFile(t, path)
		if err := os.WriteFile(path, []byte(spec), 0o644); err != nil {
			t.Fatalf("write manifest: %v", err)
		}
	}
	return root
}

// age backdates a rootfs past the grace window so it is eligible for removal.
func age(t *testing.T, root, digest string) {
	t.Helper()
	old := time.Now().Add(-imageGCGrace - time.Hour)
	if err := os.Chtimes(filepath.Join(root, "rootfs", digest), old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

// A digest a manifest still names must survive, and the stale generation
// beside it must go — with its config and ref pointer.
func TestImageGC_RemovesStaleGenerationKeepsLive(t *testing.T) {
	root := gcFixture(t,
		map[string][]string{"prod": {"rg.example.com/core:v2"}},
		map[string]string{"rg.example.com/core:v1": "old", "rg.example.com/core:v2": "new"},
		[]string{"old", "new"},
	)
	age(t, root, "old")
	age(t, root, "new")

	res, err := imageGCAt(root, false, time.Now())
	if err != nil {
		t.Fatalf("imageGCAt: %v", err)
	}
	if len(res.Removed) != 1 || res.Removed[0].Digest != "old" {
		t.Fatalf("Removed = %v, want [old]", res.Removed)
	}
	if _, err := os.Stat(filepath.Join(root, "rootfs", "new")); err != nil {
		t.Error("live digest must be kept")
	}
	if _, err := os.Stat(filepath.Join(root, "rootfs", "old")); !os.IsNotExist(err) {
		t.Error("stale rootfs must be removed")
	}
	// A ref surviving its rootfs is the trap this collector must not set: the
	// next deploy of that tag would start on a vanished lower.
	if _, err := os.Stat(filepath.Join(root, "configs", "old.json")); !os.IsNotExist(err) {
		t.Error("stale config must be removed with its rootfs")
	}
	if _, err := os.Stat(filepath.Join(root, refCacheDir, refCacheKey("rg.example.com/core:v1", runtime.GOARCH))); !os.IsNotExist(err) {
		t.Error("stale ref pointer must be removed with its rootfs")
	}
	if res.KeptLive != 1 {
		t.Errorf("KeptLive = %d, want 1", res.KeptLive)
	}
}

// Every space's manifest counts, not just one: an image live in ANY space is
// protected from a collection triggered by another.
func TestImageGC_ProtectsAcrossSpaces(t *testing.T) {
	root := gcFixture(t,
		map[string][]string{
			"shop":   {"rg.example.com/core:v2"},
			"altfab": {"rg.example.com/web:v9"},
		},
		map[string]string{"rg.example.com/core:v2": "core2", "rg.example.com/web:v9": "web9"},
		[]string{"core2", "web9"},
	)
	age(t, root, "core2")
	age(t, root, "web9")

	res, err := imageGCAt(root, false, time.Now())
	if err != nil {
		t.Fatalf("imageGCAt: %v", err)
	}
	if len(res.Removed) != 0 {
		t.Fatalf("Removed = %v, want none", res.Removed)
	}
}

// Within the grace window an unreferenced rootfs stays, so a freshly retired
// generation can still restart without a re-pull.
func TestImageGC_GracePeriodKeepsRecent(t *testing.T) {
	root := gcFixture(t, nil,
		map[string]string{"rg.example.com/core:v1": "fresh"},
		[]string{"fresh"},
	)

	res, err := imageGCAt(root, false, time.Now())
	if err != nil {
		t.Fatalf("imageGCAt: %v", err)
	}
	if len(res.Removed) != 0 {
		t.Fatalf("Removed = %v, want none within grace", res.Removed)
	}
	if res.KeptRecent != 1 {
		t.Errorf("KeptRecent = %d, want 1", res.KeptRecent)
	}
}

// A digest pinned by ref (@sha256:) resolves without any refs/ pointer.
func TestImageGC_DigestPinnedRefIsLive(t *testing.T) {
	root := gcFixture(t,
		map[string][]string{"prod": {"rg.example.com/core@sha256:pinned"}},
		nil,
		[]string{"pinned"},
	)
	age(t, root, "pinned")

	res, err := imageGCAt(root, false, time.Now())
	if err != nil {
		t.Fatalf("imageGCAt: %v", err)
	}
	if len(res.Removed) != 0 {
		t.Fatalf("Removed = %v, want none (digest-pinned ref is live)", res.Removed)
	}
}

// A dry run reports exactly what a real run would remove, and touches nothing.
func TestImageGC_DryRunRemovesNothing(t *testing.T) {
	root := gcFixture(t, nil,
		map[string]string{"rg.example.com/core:v1": "old"},
		[]string{"old"},
	)
	age(t, root, "old")

	res, err := imageGCAt(root, true, time.Now())
	if err != nil {
		t.Fatalf("imageGCAt: %v", err)
	}
	if len(res.Removed) != 1 || res.Removed[0].Digest != "old" {
		t.Fatalf("Removed = %v, want [old]", res.Removed)
	}
	if _, err := os.Stat(filepath.Join(root, "rootfs", "old")); err != nil {
		t.Error("dry run must not remove anything")
	}
	if res.FreedBytes <= 0 {
		t.Error("dry run must report the size it would free")
	}
}

// An in-flight extraction must not be collected as a stale generation.
func TestImageGC_SkipsStagingDirs(t *testing.T) {
	root := gcFixture(t, nil, nil, []string{"abc.staging"})
	age(t, root, "abc.staging")

	res, err := imageGCAt(root, false, time.Now())
	if err != nil {
		t.Fatalf("imageGCAt: %v", err)
	}
	if len(res.Removed) != 0 {
		t.Fatalf("Removed = %v, want none (staging is in flight)", res.Removed)
	}
}

// An empty or absent cache is a no-op, never an error.
func TestImageGC_EmptyCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))

	res, err := imageGCAt(filepath.Join(home, "cache", "sklp", "embedded-runtime"), false, time.Now())
	if err != nil {
		t.Fatalf("imageGCAt on empty cache: %v", err)
	}
	if len(res.Removed) != 0 {
		t.Fatalf("Removed = %v, want none", res.Removed)
	}
}

// The mount scan is the guard that protects a container whose manifest entry
// has already moved on: a digest held as an overlay lowerdir stays, even when
// unreferenced and long past the grace window.
func TestMountedLowerDigests_ParsesOwnMountTable(t *testing.T) {
	line := "558 73 0:67 / /merged rw,relatime shared:480 - overlay overlay rw," +
		"lowerdir=/cache/rootfs/livedigest/x:/cache/rootfs/otherdigest," +
		"upperdir=/up,workdir=/work"
	got, ok := cutMountOption(line, "lowerdir=")
	if !ok {
		t.Fatal("lowerdir option must be found")
	}
	want := "/cache/rootfs/livedigest/x:/cache/rootfs/otherdigest"
	if got != want {
		t.Fatalf("lowerdir = %q, want %q", got, want)
	}
}

// stamp backdates a rootfs to an explicit age so a repository's generations
// have a defined order without waiting on filesystem timestamp resolution.
func stamp(t *testing.T, root, digest string, ago time.Duration) {
	t.Helper()
	at := time.Now().Add(-ago)
	if err := os.Chtimes(filepath.Join(root, "rootfs", digest), at, at); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

// The incident this bounds: a service rolling several times an hour outpaces
// the grace window, so every generation of the day stays resident. Past the
// per-repo retention the oldest go, however fresh they are.
func TestImageGC_KeepsOnlyNewestGenerationsPerRepo(t *testing.T) {
	root := gcFixture(t,
		map[string][]string{"prod": {"rg.example.com/web:g5"}},
		map[string]string{
			"rg.example.com/web:g1": "g1",
			"rg.example.com/web:g2": "g2",
			"rg.example.com/web:g3": "g3",
			"rg.example.com/web:g4": "g4",
			"rg.example.com/web:g5": "g5",
		},
		[]string{"g1", "g2", "g3", "g4", "g5"},
	)
	for i, d := range []string{"g5", "g4", "g3", "g2", "g1"} {
		stamp(t, root, d, time.Duration(i)*time.Minute)
	}

	res, err := imageGCAt(root, false, time.Now())
	if err != nil {
		t.Fatalf("imageGCAt: %v", err)
	}
	for _, keep := range []string{"g5", "g4", "g3"} {
		if _, err := os.Stat(filepath.Join(root, "rootfs", keep)); err != nil {
			t.Errorf("%s is among the newest %d and must be kept", keep, imageGCKeepPerRepo)
		}
	}
	for _, gone := range []string{"g2", "g1"} {
		if _, err := os.Stat(filepath.Join(root, "rootfs", gone)); !os.IsNotExist(err) {
			t.Errorf("%s is past the retention and must be removed despite being recent", gone)
		}
	}
	if res.EvictedOverKeep != 2 {
		t.Errorf("EvictedOverKeep = %d, want 2", res.EvictedOverKeep)
	}
}

// Retention is counted per repository: the store is shared by every service on
// the node, so a busy service must not evict a quiet one's spare generations.
func TestImageGC_RetentionIsPerRepository(t *testing.T) {
	root := gcFixture(t,
		map[string][]string{"prod": {"rg.example.com/web:w4", "rg.example.com/api:a1"}},
		map[string]string{
			"rg.example.com/web:w1": "w1",
			"rg.example.com/web:w2": "w2",
			"rg.example.com/web:w3": "w3",
			"rg.example.com/web:w4": "w4",
			"rg.example.com/api:a1": "a1",
		},
		[]string{"w1", "w2", "w3", "w4", "a1"},
	)
	for i, d := range []string{"w4", "w3", "w2", "w1"} {
		stamp(t, root, d, time.Duration(i)*time.Minute)
	}
	// The quiet service's only generation is the oldest thing on the node.
	stamp(t, root, "a1", 30*24*time.Hour)

	if _, err := imageGCAt(root, false, time.Now()); err != nil {
		t.Fatalf("imageGCAt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "rootfs", "a1")); err != nil {
		t.Error("a live generation of another repository must survive a busy repository's rolls")
	}
	if _, err := os.Stat(filepath.Join(root, "rootfs", "w1")); !os.IsNotExist(err) {
		t.Error("the busy repository's 4th generation must be removed")
	}
}

// Retention never overrides the in-use guards: an image a manifest still names
// is kept even when newer generations of the same repository crowd it out.
func TestImageGC_RetentionNeverEvictsLive(t *testing.T) {
	root := gcFixture(t,
		map[string][]string{"prod": {"rg.example.com/web:old"}},
		map[string]string{
			"rg.example.com/web:old": "gold",
			"rg.example.com/web:n1":  "n1",
			"rg.example.com/web:n2":  "n2",
			"rg.example.com/web:n3":  "n3",
		},
		[]string{"gold", "n1", "n2", "n3"},
	)
	// The running generation is the oldest, so recency alone would evict it.
	stamp(t, root, "gold", 90*24*time.Hour)
	for i, d := range []string{"n3", "n2", "n1"} {
		stamp(t, root, d, time.Duration(i)*time.Minute)
	}

	res, err := imageGCAt(root, false, time.Now())
	if err != nil {
		t.Fatalf("imageGCAt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "rootfs", "gold")); err != nil {
		t.Fatal("the running image must never be collected, whatever its age")
	}
	if res.KeptLive != 1 {
		t.Errorf("KeptLive = %d, want 1", res.KeptLive)
	}
}

// A pointer written before the repository was recorded carries no grouping.
// Such a generation must fall back to the grace window rather than be counted
// against — or evicted by — some other service's quota.
func TestImageGC_UngroupedPointerFallsBackToGrace(t *testing.T) {
	root := gcFixture(t, nil,
		map[string]string{"rg.example.com/web:v1": "legacy"},
		[]string{"legacy"},
	)
	// Rewrite the pointer in the pre-repository format.
	key := refCacheKey("rg.example.com/web:v1", runtime.GOARCH)
	if err := os.WriteFile(filepath.Join(root, refCacheDir, key), []byte("legacy"), 0o644); err != nil {
		t.Fatalf("write legacy pointer: %v", err)
	}

	res, err := imageGCAt(root, false, time.Now())
	if err != nil {
		t.Fatalf("imageGCAt: %v", err)
	}
	if len(res.Removed) != 0 {
		t.Fatalf("Removed = %v, want none within grace", res.Removed)
	}

	age(t, root, "legacy")
	res, err = imageGCAt(root, false, time.Now())
	if err != nil {
		t.Fatalf("imageGCAt: %v", err)
	}
	if len(res.Removed) != 1 || res.Removed[0].Digest != "legacy" {
		t.Fatalf("Removed = %v, want [legacy] once past the grace window", res.Removed)
	}
}
