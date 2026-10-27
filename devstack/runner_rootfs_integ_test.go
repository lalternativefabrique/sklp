//go:build linux

package devstack

// Integration test for the G4 wiring: the embedded backend builds a runner
// rootfs from a recipe (packages baked) and memoises it by hash. It exercises
// the real path — runtime.PrepareImage (daemonless pull) + runnerbuild.Build
// (copy + apt via unshare) — so it needs rootless userns + network + a subuid
// range; it skips otherwise. The pure detection (devServiceSpec) is covered by
// the unit tests; this proves the build actually materialises the rootfs.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/internal/integtest"
	"github.com/lalternativefabrique/sklp/pipeline/runnerimage"
)

func skipUnlessBuildable(t *testing.T) { integtest.SkipUnlessRootlessBuild(t) }

// TestEmbeddedBackend_BuildsRunnerRootfsWithPackages proves the backend builds a
// rootfs from a recipe declaring `packages: [git]` and that git lands in it —
// the end-to-end G4 path minus the runc launch (covered by the launcher tests).
// A second call returns the memoised result without rebuilding.
func TestEmbeddedBackend_BuildsRunnerRootfsWithPackages(t *testing.T) {
	skipUnlessBuildable(t)
	if testing.Short() {
		t.Skip("pulls a base image + runs apt; skipped in -short")
	}

	b, ok := chooseDevBackend("sklp-dev-runnerbuild-test", false)
	if !ok {
		t.Skip("embedded backend unavailable on this host")
	}
	eb := b.(*embeddedBackend)
	defer eb.Close()

	recipe := runnerimage.Runner{
		BaseImage: "debian:trixie-slim",
		Packages:  []string{"git"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	res, err := eb.runnerRootfs(ctx, recipe)
	if err != nil {
		t.Fatalf("runnerRootfs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(res.Rootfs, "usr", "bin", "git")); err != nil {
		t.Fatalf("git not baked into runner rootfs: %v", err)
	}

	// Memoised: a second call returns the same rootfs path, no rebuild.
	res2, err := eb.runnerRootfs(ctx, recipe)
	if err != nil {
		t.Fatalf("second runnerRootfs: %v", err)
	}
	if res2.Rootfs != res.Rootfs {
		t.Fatalf("memoisation broken: %q != %q", res2.Rootfs, res.Rootfs)
	}
}
