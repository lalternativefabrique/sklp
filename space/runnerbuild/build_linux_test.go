//go:build linux

package runnerbuild

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/pipeline/runnerimage"
)

// fakeSteps records the rootfs + runs it was asked to execute and, optionally,
// drops a marker file into the rootfs to stand in for a real step's side effect.
type fakeSteps struct {
	calls   int
	gotRuns []string
	gotEnv  []string // env handed to WithStage before Exec
	marker  string   // if set, written into rootfsDir on Exec
}

func (f *fakeSteps) Exec(_ context.Context, rootfsDir string, runs []string) error {
	f.calls++
	f.gotRuns = runs
	if f.marker != "" {
		return os.WriteFile(filepath.Join(rootfsDir, f.marker), []byte("ok"), 0o644)
	}
	return nil
}

// WithStage records the per-stage env so a test can assert the recipe ENV
// (e.g. GOTOOLCHAIN=local) reaches the step runner.
func (f *fakeSteps) WithStage(env []string, _ []CacheMount) StepRunner {
	f.gotEnv = env
	return f
}

// fakeBase writes a minimal "base rootfs" on disk so copyTree has something real
// to copy (cp -a needs a populated tree).
func fakeBase(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "usr", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "usr", "bin", "sh"), []byte("#!stub"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func recipe() runnerimage.Runner {
	return runnerimage.Runner{
		BaseImage: "base:latest",
		Packages:  []string{"git"},
	}
}

// TestBuild_CopiesBaseAndRunsSteps: a fresh build copies the base rootfs, runs
// the plan's steps, and writes the .built sentinel.
func TestBuild_CopiesBaseAndRunsSteps(t *testing.T) {
	cache := t.TempDir()
	base := fakeBase(t)
	steps := &fakeSteps{marker: "step-ran"}

	res, err := Build(context.Background(), cache, base, recipe(), nil, steps)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if steps.calls != 1 {
		t.Errorf("steps ran %d times, want 1", steps.calls)
	}
	// Base content copied through.
	if _, err := os.Stat(filepath.Join(res.Rootfs, "usr", "bin", "sh")); err != nil {
		t.Errorf("base content not copied into rootfs: %v", err)
	}
	// Step side effect present.
	if _, err := os.Stat(filepath.Join(res.Rootfs, "step-ran")); err != nil {
		t.Errorf("step marker not in rootfs: %v", err)
	}
	// Sentinel written.
	hash, _ := Hash(recipe())
	if _, err := os.Stat(filepath.Join(cache, "runner", hash, ".built")); err != nil {
		t.Errorf(".built sentinel missing: %v", err)
	}
	// Env/workdir metadata carried.
	if res.Workdir != "/workspace" {
		t.Errorf("Workdir = %q, want /workspace", res.Workdir)
	}
}

// TestBuild_AppliesRecipeEnvToSteps: the recipe's baked ENV must reach the step
// runner via WithStage before the RUN steps execute. Regression for the space
// rootfs build inheriting the host GOTOOLCHAIN=auto and `go install` re-execing
// into an absent toolchain dir ("cannot find GOROOT directory").
func TestBuild_AppliesRecipeEnvToSteps(t *testing.T) {
	cache := t.TempDir()
	base := fakeBase(t)
	steps := &fakeSteps{}

	if _, err := Build(context.Background(), cache, base, recipe(), nil, steps); err != nil {
		t.Fatalf("Build: %v", err)
	}
	var seen bool
	for _, kv := range steps.gotEnv {
		if kv == "GOTOOLCHAIN=local" {
			seen = true
		}
	}
	if !seen {
		t.Errorf("GOTOOLCHAIN=local not applied to step runner; got env %v", steps.gotEnv)
	}
}

// TestBuild_CacheHitSkipsSteps: a second Build with the sentinel present must
// NOT copy or run steps again.
func TestBuild_CacheHitSkipsSteps(t *testing.T) {
	cache := t.TempDir()
	base := fakeBase(t)

	if _, err := Build(context.Background(), cache, base, recipe(), nil, &fakeSteps{}); err != nil {
		t.Fatalf("first Build: %v", err)
	}
	second := &fakeSteps{}
	if _, err := Build(context.Background(), cache, base, recipe(), nil, second); err != nil {
		t.Fatalf("second Build: %v", err)
	}
	if second.calls != 0 {
		t.Errorf("cache hit still ran steps %d times, want 0", second.calls)
	}
}

// slowSteps blocks in Exec until released, so a test can force two Build
// calls for the same hash to overlap in time.
type slowSteps struct {
	release chan struct{}
	calls   int
}

func (s *slowSteps) Exec(_ context.Context, rootfsDir string, _ []string) error {
	s.calls++
	<-s.release
	return os.WriteFile(filepath.Join(rootfsDir, "step-ran"), []byte("ok"), 0o644)
}

func (s *slowSteps) WithStage([]string, []CacheMount) StepRunner { return s }

// TestBuild_ConcurrentSameHashSerialises is the regression guard for the race
// --concurrency exposed in production: two runs for the SAME project started
// close together both saw a cache miss (no .built sentinel yet) and both ran
// os.RemoveAll(rootfs) + copyTree concurrently, each wiping the other's
// in-progress rootfs — surfacing as "dpkg/status: No such file or directory"
// and "chroot: … /bin/sh: No such file or directory" in production logs.
//
// A single StepRunner shared by both callers proves the fix: if Build let two
// callers into the build section for the same hash, both would call Exec
// (calls == 2, or a step could observe a rootfs the other just removed). With
// the per-hash lock, the second caller blocks until the first's sentinel
// exists and takes the cache-hit path instead — Exec runs exactly once.
func TestBuild_ConcurrentSameHashSerialises(t *testing.T) {
	cache := t.TempDir()
	base := fakeBase(t)
	steps := &slowSteps{release: make(chan struct{})}

	type outcome struct {
		res Result
		err error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			res, err := Build(context.Background(), cache, base, recipe(), nil, steps)
			results <- outcome{res, err}
		}()
	}
	close(start)

	// Give both goroutines a chance to reach the lock before releasing the
	// step: without the fix this window is exactly where the race fired.
	time.Sleep(50 * time.Millisecond)
	close(steps.release)

	for range 2 {
		o := <-results
		if o.err != nil {
			t.Fatalf("Build: %v", o.err)
		}
		if _, err := os.Stat(filepath.Join(o.res.Rootfs, "step-ran")); err != nil {
			t.Errorf("built rootfs missing the step's output: %v", err)
		}
	}
	if steps.calls != 1 {
		t.Errorf("Exec ran %d times for one hash under concurrent Build, want 1", steps.calls)
	}
}

// TestBuild_StepFailureNoSentinel: if a step fails, no sentinel is written, so
// the next run rebuilds.
func TestBuild_StepFailureNoSentinel(t *testing.T) {
	cache := t.TempDir()
	base := fakeBase(t)

	_, err := Build(context.Background(), cache, base, recipe(), nil, failSteps{})
	if err == nil {
		t.Fatal("expected Build to fail when a step fails")
	}
	hash, _ := Hash(recipe())
	if _, err := os.Stat(filepath.Join(cache, "runner", hash, ".built")); err == nil {
		t.Error("sentinel written despite step failure")
	}
}

type failSteps struct{}

func (failSteps) Exec(context.Context, string, []string) error  { return errStep }
func (f failSteps) WithStage([]string, []CacheMount) StepRunner { return f }

var errStep = &stepErr{}

type stepErr struct{}

func (*stepErr) Error() string { return "boom" }

// TestBakeAwareHash pins the cache-key contract: no bake leaves the recipe hash
// untouched; adding/changing a bake (e.g. baking sklp where only skt was baked)
// produces a different key so a stale rootfs is never reused; an identical bake
// set is stable.
func TestBakeAwareHash(t *testing.T) {
	dir := t.TempDir()
	skt := filepath.Join(dir, "skt")
	sklp := filepath.Join(dir, "sklp")
	if err := os.WriteFile(skt, []byte("skt-bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sklp, []byte("sklp-bin"), 0o755); err != nil {
		t.Fatal(err)
	}

	const recipeHash = "abc123def456"
	bare, err := bakeAwareHash(recipeHash, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bare != recipeHash {
		t.Errorf("no bake must pass the recipe hash through, got %q", bare)
	}

	sktOnly, err := bakeAwareHash(recipeHash, []BakeFile{{Src: skt, Dest: "/usr/local/bin/skt", Mode: 0o755}})
	if err != nil {
		t.Fatal(err)
	}
	bothBakes := []BakeFile{
		{Src: skt, Dest: "/usr/local/bin/skt", Mode: 0o755},
		{Src: sklp, Dest: "/usr/local/bin/sklp", Mode: 0o755},
	}
	withSklp, err := bakeAwareHash(recipeHash, bothBakes)
	if err != nil {
		t.Fatal(err)
	}
	if sktOnly == recipeHash {
		t.Error("a bake must change the hash off the bare recipe hash")
	}
	if withSklp == sktOnly {
		t.Error("adding the sklp bake must bust the cache key")
	}

	// Order-independent + stable: same set in a different order → same key.
	reordered, err := bakeAwareHash(recipeHash, []BakeFile{bothBakes[1], bothBakes[0]})
	if err != nil {
		t.Fatal(err)
	}
	if reordered != withSklp {
		t.Errorf("hash must be order-independent: %q != %q", reordered, withSklp)
	}

	// Content-addressed, not size+mtime: a rebuilt sklp with NEW bytes but the
	// SAME size and mtime (reproducible builds routinely keep both) must bust the
	// key. Under the old size+mtime hash this collided, so the runner reused a
	// rootfs baking a stale sklp (e.g. one without -env-file). Overwrite in place
	// to preserve size, then restore the original mtime to defeat any mtime check.
	fi, err := os.Stat(sklp)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sklp, []byte("SKLP-BIN"), 0o755); err != nil { // same length as "sklp-bin"
		t.Fatal(err)
	}
	if err := os.Chtimes(sklp, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	newContent, err := bakeAwareHash(recipeHash, bothBakes)
	if err != nil {
		t.Fatal(err)
	}
	if newContent == withSklp {
		t.Error("a rebuilt baked binary with same size+mtime but new content must bust the cache key")
	}
}
