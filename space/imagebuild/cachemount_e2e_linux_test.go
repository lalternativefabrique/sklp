//go:build linux

package imagebuild

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	parse "github.com/lalternativefabrique/sklp/pipeline/imagebuild"
	"github.com/lalternativefabrique/sklp/space/runnerbuild"
)

// subidRemoveAllForTest drops a dir holding subuid-owned files, which the test
// uid cannot unlink directly. Mirrors the production cleanup path.
func subidRemoveAllForTest(dir string) {
	cmd := exec.Command("unshare",
		"--user", "--map-root-user",
		"--map-users=1:100000:65535", "--map-groups=1:100000:65535",
		"--mount", "rm", "-rf", dir)
	if err := cmd.Run(); err != nil {
		_ = os.RemoveAll(dir)
	}
}

// TestRuncStepRunner_E2E_GoModCacheIsMounted is the regression proof for the
// silently-missed module cache: golang:* ships GOPATH=/go in its image env, so
// a RUN writing Go modules landed in /go/pkg/mod inside the throwaway rootfs
// while the persistent cache sat unused at /root/go/pkg/mod. The symptom was a
// build cache stuck at 4K next to a 994M go-build cache, and every publish
// re-downloading the whole module graph over the network.
//
// It asserts on the HOST cache dir: the bind mount is what makes the write
// survive the container, so content appearing there is the only proof the
// toolchain wrote through the mount rather than into the rootfs.
func TestRuncStepRunner_E2E_GoModCacheIsMounted(t *testing.T) {
	skipUnlessRuncE2E(t)

	cache := t.TempDir()
	work := t.TempDir()

	// NOT t.TempDir(): the cache fills with files owned by the container's
	// mapped subuid, which the test uid cannot unlink — Go's cleanup would fail
	// the test after it already passed. Own the dir and drop it through the
	// same userns remap the real cache clean uses.
	buildCache, err := os.MkdirTemp("", "sklp-gomodcache-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { subidRemoveAllForTest(buildCache) })

	// `go mod download` on a real module: the smallest RUN that exercises
	// GOMODCACHE end-to-end against the same base the publish path uses.
	df, err := parse.Parse(`FROM golang:1.25.12
RUN mkdir -p /src && cd /src && go mod init probe && go get github.com/pkg/errors@v0.9.1
`)
	if err != nil {
		t.Fatal(err)
	}

	_, err = Build(context.Background(), cache, work, df,
		Options{ContextDir: t.TempDir(), CacheDir: buildCache, Progress: os.Stderr},
		runnerbuild.RuncStepRunner{CacheRoot: cache, Stdout: os.Stderr, Stderr: os.Stderr})
	if err != nil {
		t.Fatalf("build via runc: %v", err)
	}

	// The downloaded module must have landed in the HOST module cache. Pre-fix
	// this dir stayed empty because the image's GOPATH won.
	goMod := filepath.Join(buildCache, "go-mod")
	entries, err := os.ReadDir(goMod)
	if err != nil {
		t.Fatalf("host module cache %s unreadable: %v", goMod, err)
	}
	if len(entries) == 0 {
		t.Fatalf("host module cache %s is empty: the toolchain wrote past the bind mount "+
			"(the image's GOPATH won over GOMODCACHE)", goMod)
	}
}
