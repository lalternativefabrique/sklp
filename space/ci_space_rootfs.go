package space

import (
	"context"
	"fmt"
	"github.com/lalternativefabrique/sklp/space/host"
	"os"
	"runtime"

	"github.com/lalternativefabrique/sklp/internal/toolchain"
	"github.com/lalternativefabrique/sklp/pipeline/runnerimage"
	"github.com/lalternativefabrique/sklp/space/runnerbuild"
)

// defaultSpaceBaseImage is the base layered with the space.yaml toolchain when
// the recipe declares no base_image. Mirrors `sklp space` and the dev backend.
const defaultSpaceBaseImage = host.BaseImage

// buildSpaceRootfs materialises the CI runner rootfs from .sklp/space.yaml the
// SAME way `sklp space` and `sklp dev` do — base image + apt packages + toolchain
// + skt tools, built daemonless and cached by recipe hash — instead of pulling a
// published runner image. This kills the version drift that bit us: a stale
// :latest baked an old sklp / toolchain, so a `run:` service or a `go`/`node`
// step ran against tools that no longer matched the repo. Now every run uses the
// recipe's exact tool set, with the host skt baked so `RUN skt install …`
// resolves. Returns the rootfs dir, its baked env (KEY=value), and the workdir.
// buildSpaceRootfs builds (or reuses) the space rootfs from the repo's recipe.
// out receives the build's progress — apt, toolchain installs — and defaults
// to stderr; the CI path hands it the run's live output so a caller watching
// the run sees the minutes the rootfs takes instead of a blank screen.
func buildSpaceRootfs(ctx context.Context, space *Space, repoPath, file string, out *os.File) (rootfs string, env []string, workdir string, unlock func(), err error) {
	unlock = func() {}
	if out == nil {
		out = os.Stderr
	}
	// Locate the recipe: an explicit --file wins, else the .sklp/space.yaml lookup.
	recipePath := file
	if recipePath == "" {
		recipePath = runnerimage.ResolvePath(repoPath)
	}
	if _, serr := os.Stat(recipePath); serr != nil {
		// No recipe and no --image: tell the user how to open a space, instead of
		// the cryptic "open .sklp/space.yaml: no such file or directory".
		return "", nil, "", unlock, fmt.Errorf("no space recipe at %s: open a bare space with `sklp space up <name> --image <ref>`, or add a .sklp/space.yaml (or pass --file)", recipePath)
	}
	cfg, err := runnerimage.Load(recipePath)
	if err != nil {
		return "", nil, "", unlock, fmt.Errorf("load space recipe: %w", err)
	}
	r := cfg.Runner
	// Force a concrete base so the recipe renders in from-base mode (FROM <base>
	// + RUN layers); an empty base_image routes to the legacy from-source path.
	if r.BaseImage == "" {
		if v := os.Getenv("SKLP_DEV_BASE_IMAGE"); v != "" {
			r.BaseImage = v
		} else {
			r.BaseImage = defaultSpaceBaseImage
		}
	}

	baseRootfs, err := prepareImage(ctx, space.CacheRoot(), r.BaseImage, runtime.GOARCH)
	if err != nil {
		return "", nil, "", unlock, fmt.Errorf("prepare base %q: %w", r.BaseImage, err)
	}

	// Bake the host skt so the recipe's `RUN skt install …` steps resolve on a
	// plain base that ships none.
	var bake []runnerbuild.BakeFile
	if sktPath, serr := toolchain.EnsureSkt(); serr == nil {
		bake = append(bake, runnerbuild.BakeFile{Src: sktPath, Dest: runnerimage.SktBinPath(), Mode: 0o755})
	}
	// Bake the CURRENT sklp (this very binary) at /usr/local/bin/sklp so the
	// runner exec'd inside the rootfs is the same version that built it — the
	// whole point of building on the fly instead of pulling a stale image.
	if self, serr := os.Executable(); serr == nil {
		bake = append(bake, runnerbuild.BakeFile{Src: self, Dest: "/usr/local/bin/sklp", Mode: 0o755})
	}

	res, err := runnerbuild.Build(ctx, space.CacheRoot(), baseRootfs, r, bake, runnerbuild.UnshareStepRunner{
		Stdout: out,
		Stderr: out,
	})
	if err != nil {
		res.Unlock()
		return "", nil, "", unlock, fmt.Errorf("build space rootfs: %w", err)
	}
	return res.Rootfs, bakedEnvSliceRT(res), res.Workdir, res.Unlock, nil
}

// bakedEnvSliceRT renders a built rootfs's baked ENV as KEY=value entries in
// declaration order (same as the cmd/devstack copies; kept here to avoid an
// import of either).
func bakedEnvSliceRT(res runnerbuild.Result) []string {
	out := make([]string, 0, len(res.Env))
	for _, k := range res.EnvOrder {
		out = append(out, k+"="+res.Env[k])
	}
	return out
}
