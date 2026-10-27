//go:build linux

// Package runnerbuild builds a project's runner ROOTFS in pure Go, daemonless.
// It does NOT produce an OCI image: the embedded runtime consumes an extracted
// rootfs dir (runtime.PrepareImage), so the builder materialises the runner
// recipe's RUN steps onto a copy of the base rootfs and hands back that dir for
// runc to mount. The runner Dockerfile is generated, linear and COPY-free
// (runnerimage.renderFromBase), so its RUN steps are just shell commands run in
// the rootfs — no BuildKit, no layer-commit machinery.
//
// Materialisation strategy (mirrors buildah's rootless storage choice):
//   - DEFAULT: a flat COPY of the base rootfs (the "vfs" approach). Robust, no
//     dependency, and — crucially — a single filesystem, so dpkg's cross-dir
//     rename(2) during `apt-get install` does not hit EXDEV. A rootless KERNEL
//     overlay cannot serve apt: redirect_dir (needed for lower-dir renames) is
//     refused to a non-root userns, so dpkg fails with "Invalid cross-device
//     link". fuse-overlayfs would fix that but is an extra dependency; copy is
//     the dependency-free default. (A fuse-overlayfs fast path is a future opt.)
package runnerbuild

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/lalternativefabrique/sklp/pipeline/runnerimage"
)

// buildLocks gates concurrent Build calls for the SAME recipe hash. Under
// --concurrency > 1, two runs for the same project can both reach Build
// before either has written the .built sentinel; without a lock both see a
// cache miss and race on the identical rootfs dir — one's os.RemoveAll wipes
// the tree the other is mid-copy-into or mid-chroot-into, surfacing as
// "Could not open file /var/lib/dpkg/status" or "chroot: … /bin/sh: No such
// file or directory" depending on which step lost the race. Keyed by hash
// (not a single global lock) so unrelated recipes still build in parallel —
// only same-recipe builds serialise, and only the ones that were actually
// going to collide.
var buildLocks sync.Map // hash (string) -> *sync.Mutex

func lockForHash(hash string) *sync.Mutex {
	v, _ := buildLocks.LoadOrStore(hash, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// bakeAwareHash folds the baked file set into the recipe hash so a changed bake
// (different dest, or a rebuilt source binary) produces a new cache key. Each
// bake contributes dest + mode + the source's CONTENT digest; sources are sorted
// by dest for a stable result. A missing source is hashed by dest alone (the
// build will fail later with a clear error, not silently reuse a stale rootfs).
//
// Content, not size+mtime: `go build` is reproducible, so a rebuilt sklp with new
// code is frequently byte-for-byte the SAME size, and its mtime may match the
// cached rootfs's source — under size+mtime the recompiled sklp reused a stale
// rootfs that baked the OLD binary (e.g. one predating the `-env-file` flag),
// surfacing as "flag provided but not defined" from the inner runner.
func bakeAwareHash(recipeHash string, bake []BakeFile) (string, error) {
	if len(bake) == 0 {
		return recipeHash, nil
	}
	sorted := append([]BakeFile{}, bake...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Dest < sorted[j].Dest })
	h := sha256.New()
	fmt.Fprintln(h, recipeHash)
	for _, f := range sorted {
		fmt.Fprintf(h, "%s\t%o\t", f.Dest, f.Mode)
		digest, err := fileSHA256(f.Src)
		if err != nil {
			fmt.Fprintln(h, "missing")
			continue
		}
		fmt.Fprintf(h, "%s\n", digest)
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}

// fileSHA256 streams a file through sha256 so a baked binary's cache key tracks
// its exact bytes (reproducible builds can keep size+mtime identical).
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Result is what a successful build yields: the rootfs dir runc mounts, plus the
// ENV/WORKDIR metadata to fold onto the run: service (these are NOT written into
// the rootfs — they are process config the launcher applies).
type Result struct {
	Rootfs   string            // the built rootfs dir, ready as a runc rootfs
	Env      map[string]string // baked process env (ENV instructions)
	EnvOrder []string          // ENV keys in declaration order
	Workdir  string            // WORKDIR ("" if none)
	Hash     string            // cache key of this rootfs under <cacheRoot>/runner/
	// Unlock releases the collector pin Build took on the rootfs. Never nil.
	// Callers that mount Rootfs must defer it until the last container using it
	// is gone, or `sklp cache gc` may reclaim the tree from under a live run.
	Unlock func()
}

// StepRunner executes the recipe's RUN commands inside the rootfs at rootfsDir.
// It owns the isolation: it maps the caller to root (+ a subuid range so dpkg
// can chown to service uids) in a user namespace and chroots into rootfsDir for
// each step. Abstracted so the production runner (runc + space egress) and the
// MVP runner (unshare + chroot, host net) are swappable, and so the build flow
// is testable with a fake.
type StepRunner interface {
	// Exec must abort its child on ctx cancellation: a build is the longest
	// blocking phase of a run, and an unkillable one keeps the caller's teardown
	// defers from firing on Ctrl-C, stranding detached service containers.
	Exec(ctx context.Context, rootfsDir string, runs []string) error
	// WithStage returns a runner configured for one build stage: the base
	// image env (so the image PATH takes effect) plus that stage's cache
	// mounts. imagebuild.Build calls it before each stage so the concrete
	// runner type stays opaque to the build flow (no type switch). A runner
	// that needs no per-stage config (a test fake) returns itself.
	WithStage(imageEnv []string, cacheMounts []CacheMount) StepRunner
}

// layout is the on-disk shape of one runner rootfs keyed by recipe hash:
//
//	<cacheRoot>/runner/<hash>/rootfs   the built rootfs (base copy + recipe delta)
//	<cacheRoot>/runner/<hash>/.built   sentinel: build completed
type layout struct{ rootfs, sentinel string }

func layoutFor(cacheRoot, hash string) layout {
	root := filepath.Join(cacheRoot, "runner", hash)
	return layout{
		rootfs:   filepath.Join(root, "rootfs"),
		sentinel: filepath.Join(root, ".built"),
	}
}

// BakeFile is a host file copied into the rootfs before the RUN steps execute.
// It is how a tool the steps invoke but the base image lacks gets injected — in
// `sklp dev`, the host `skt` binary baked at /usr/local/bin/skt so `RUN skt
// install …` works on a plain base (a published runner base ships skt itself).
type BakeFile struct {
	Src  string // host path
	Dest string // path inside the rootfs (leading slash, e.g. /usr/local/bin/skt)
	Mode os.FileMode
}

// Build materialises the runner recipe r onto a copy of the base rootfs at
// baseRootfs, returning the built rootfs and its env/workdir. It is idempotent
// per recipe hash: a completed build (.built present) returns the cached rootfs
// without copying or re-running any step. bake injects host files (e.g. skt)
// into the rootfs before the steps; steps executes the RUN commands.
func Build(ctx context.Context, cacheRoot, baseRootfs string, r runnerimage.Runner, bake []BakeFile, steps StepRunner) (Result, error) {
	// Every failure returns this, so `defer res.Unlock()` is safe on a caller
	// that does not branch on err first.
	failed := Result{Unlock: func() {}}
	recipeHash, err := runnerimage.RootfsHash(r)
	if err != nil {
		return failed, err
	}
	// Fold the baked files into the cache key: the recipe hash alone ignores
	// them, so a changed bake set (e.g. baking sklp where a prior build baked
	// only skt) would silently reuse a stale rootfs. Hash each bake's dest +
	// source size + mtime so any swap busts the cache.
	hash, err := bakeAwareHash(recipeHash, bake)
	if err != nil {
		return failed, err
	}
	plan, err := runnerimage.RunnerBuildPlan(r)
	if err != nil {
		return failed, err
	}
	lo := layoutFor(cacheRoot, hash)

	res := Result{
		Rootfs:   lo.rootfs,
		Env:      plan.Env,
		EnvOrder: plan.EnvOrder,
		Workdir:  plan.Workdir,
		Hash:     hash,
		Unlock:   Lock(cacheRoot, hash),
	}

	// Serialise everything below per hash: the sentinel check, the build, and
	// the sentinel write must all happen under the same holder, or two
	// concurrent callers for this hash both pass the check and race on the
	// rootfs dir (see buildLocks' doc comment).
	lock := lockForHash(hash)
	lock.Lock()
	defer lock.Unlock()

	// Cache hit: the recipe already built (by us, just now, or an earlier
	// call before this lock existed). The rootfs is on disk; return it.
	if _, err := os.Stat(lo.sentinel); err == nil {
		MarkUsed(cacheRoot, hash, time.Now())
		return res, nil
	}

	// Fresh build. Remove any partial rootfs from a previous aborted run so a
	// half-copied tree never masquerades as a base. A prior run's steps may
	// have chowned files to mapped subuid owners (e.g. Go's read-only modcache
	// under a RUN step), so this goes through the same userns-aware removal as
	// the other teardown paths rather than a plain RemoveAll.
	if rerr := removeBuiltRootfs(lo.rootfs); rerr != nil {
		res.Unlock()
		return failed, rerr
	}
	if err := os.MkdirAll(filepath.Dir(lo.rootfs), 0o755); err != nil {
		res.Unlock()
		return failed, err
	}

	// Materialise: a flat copy of the base rootfs (single fs => no dpkg EXDEV).
	if err := copyTree(baseRootfs, lo.rootfs); err != nil {
		res.Unlock()
		return failed, fmt.Errorf("runnerbuild: copy base rootfs: %w", err)
	}

	// Inject host tools the steps need but the base lacks (e.g. skt). Done after
	// the base copy, before the steps, so `RUN skt install …` resolves them.
	for _, f := range bake {
		if err := bakeFile(lo.rootfs, f); err != nil {
			_ = removeBuiltRootfs(lo.rootfs)
			res.Unlock()
			return failed, fmt.Errorf("runnerbuild: bake %s: %w", f.Dest, err)
		}
	}

	// Apply the recipe's ENV (e.g. GOTOOLCHAIN=local) to the step runner before
	// executing, so the RUN steps run with the baked image env — not just the
	// hardcoded PATH. Without this the chroot inherits the host GOTOOLCHAIN=auto
	// and `skt`'s `go install` re-execs into a downloaded toolchain dir absent
	// from the rootfs ("cannot find GOROOT directory"). Mirrors imagebuild.Build's
	// per-stage WithStage. No cache mounts here: the runner rootfs build is the
	// cached artifact itself (keyed by recipe hash).
	steps = steps.WithStage(planEnvSlice(plan), nil)

	if err := steps.Exec(ctx, lo.rootfs, plan.Runs); err != nil {
		// Drop the partial rootfs immediately: a failed build otherwise leaves a
		// full base copy (~100s of MB) on disk until the next build of the same
		// hash reclaims it. Cleanup runs in a userns so it can remove files the
		// steps chowned to mapped (subuid) owners. Best-effort.
		_ = removeBuiltRootfs(lo.rootfs)
		res.Unlock()
		return failed, fmt.Errorf("runnerbuild: build steps: %w", err)
	}

	if err := os.WriteFile(lo.sentinel, []byte(hash+"\n"), 0o644); err != nil {
		res.Unlock()
		return failed, err
	}
	MarkBuilt(cacheRoot, hash, time.Now())
	return res, nil
}

// removeBuiltRootfs deletes a (possibly partially) built rootfs. Build steps run
// as root in a userns and chown files to mapped subuid owners (e.g. 100000+),
// which the host uid cannot unlink directly. So the removal runs in the SAME
// userns mapping the steps used, where those owners map back to root. Falls back
// to a plain RemoveAll (covers the no-subuid / nothing-chowned case).
func removeBuiltRootfs(dir string) error {
	cmd := exec.Command("unshare",
		"--user", "--map-root-user",
		"--map-users=1:100000:65535", "--map-groups=1:100000:65535",
		"--mount", "rm", "-rf", dir)
	if err := cmd.Run(); err != nil {
		_ = os.RemoveAll(dir)
	}
	// Both removals are best-effort, so verify rather than trust: a rootfs
	// left standing here is copied OVER on the next build, and the base copy
	// then fails file by file ("cp: cannot create regular file … File
	// exists") for thousands of paths — the real error buried under the
	// flood. Reporting it lets the caller stop with one clear message.
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("runnerbuild: could not remove the previous rootfs at %s "+
			"(files owned by mapped subuids?); remove it with `sklp clean` and retry", dir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("runnerbuild: stat previous rootfs %s: %w", dir, err)
	}
	return nil
}

// bakeFile copies one host file into the rootfs at f.Dest, creating parent dirs.
// Used to inject a tool (skt) the RUN steps invoke but the base image omits.
func bakeFile(rootfs string, f BakeFile) error {
	dst := filepath.Join(rootfs, f.Dest)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(f.Src)
	if err != nil {
		return err
	}
	defer in.Close()
	mode := f.Mode
	if mode == 0 {
		mode = 0o755
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// copyTree copies the base rootfs to dst preserving ownership, modes, symlinks
// and xattrs. cp -a is used rather than a Go walk because the base rootfs holds
// hardlinks and special files that cp handles correctly and a naive walk would
// mangle; the runtime already shells out to system tools (mount, runc) so this
// adds no new class of dependency.
//
// /dev is EXCLUDED: rootless cp cannot read the base rootfs's char-device nodes
// (/dev/null, /dev/zero, … are mode 0000 specials a non-root user may not open),
// and copying them is pointless — the runtime mounts a fresh /dev (+ /dev/pts)
// at `runc create`. We re-create /dev as an empty mountpoint dir instead.
func copyTree(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	// glob src's top-level entries and copy all but /dev. `cp -a <entries> dst`
	// preserves the copy-contents-into-dst behaviour without touching /dev.
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	args := []string{"-a"}
	for _, e := range entries {
		if e.Name() == "dev" {
			continue // mounted fresh at runc create; specials are unreadable rootless
		}
		args = append(args, filepath.Join(src, e.Name()))
	}
	args = append(args, dst)
	if out, err := exec.Command("cp", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, string(out))
	}
	// Empty /dev mountpoint for the runtime's devtmpfs/bind mounts.
	return os.MkdirAll(filepath.Join(dst, "dev"), 0o755)
}

// Hash exposes the recipe's rootfs cache key (delegates to runnerimage).
func Hash(r runnerimage.Runner) (string, error) { return runnerimage.RootfsHash(r) }

// planEnvSlice renders the plan's baked ENV as KEY=value entries in declaration
// order, for WithStage. GOTOOLCHAIN=local lives here — the build steps must see
// it so `go install` never re-execs into an absent toolchain dir.
func planEnvSlice(plan runnerimage.BuildPlan) []string {
	out := make([]string, 0, len(plan.EnvOrder))
	for _, k := range plan.EnvOrder {
		out = append(out, k+"="+plan.Env[k])
	}
	return out
}
