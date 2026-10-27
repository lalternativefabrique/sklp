//go:build linux

package imagebuild

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	parse "github.com/lalternativefabrique/sklp/pipeline/imagebuild"
	"github.com/lalternativefabrique/sklp/space/runnerbuild"
)

// TestRuncStepRunner_E2E_BunCacheIsMounted is the bun counterpart of the
// GOMODCACHE proof: assert on the HOST cache dir, because the bind mount is
// what makes a download survive the container. Content landing there is the
// only evidence bun wrote THROUGH the mount instead of into the throwaway
// rootfs — the failure mode that left the module cache empty for every Go
// build until GOMODCACHE was pointed at the mount.
//
// Unlike pnpm, bun's cache is a plain download cache with no same-filesystem
// hard-link requirement, so a host bind mount is a valid target for it.
//
// The base deliberately does NOT ship bun: oven/bun:* already exports
// BUN_INSTALL_CACHE_DIR at exactly our mount target, so it passes with or
// without the export and proves nothing. Installing bun ourselves leaves the
// cache location to bun's $HOME-derived default, which is what our export has
// to override — the same shape as the golang:* GOPATH=/go case.
func TestRuncStepRunner_E2E_BunCacheIsMounted(t *testing.T) {
	skipUnlessRuncE2E(t)

	cache := t.TempDir()
	work := t.TempDir()

	// NOT t.TempDir(): TMPDIR is a tmpfs here, and a bind mount reflects its
	// SOURCE filesystem — the container would get a tmpfs and the assertion
	// would read a RAM-backed dir. It also fills with subuid-owned files the
	// test uid cannot unlink, so Go's own cleanup would fail the test after it
	// already passed. Keep it on the real disk and drop it via the userns remap.
	buildCache, err := os.MkdirTemp(os.Getenv("HOME")+"/.cache", "sklp-buncache-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { subidRemoveAllForTest(buildCache) })

	df, err := parse.Parse(`FROM alpine:3.20
RUN apk add --no-cache bash curl unzip libstdc++ && curl -fsSL https://bun.sh/install | bash
RUN mkdir -p /src && cd /src && /root/.bun/bin/bun init -y >/dev/null 2>&1 && /root/.bun/bin/bun add is-odd@3.0.1
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

	bunCache := filepath.Join(buildCache, "bun")
	entries, err := os.ReadDir(bunCache)
	if err != nil {
		t.Fatalf("host bun cache %s unreadable: %v", bunCache, err)
	}
	if len(entries) == 0 {
		t.Fatalf("host bun cache %s is empty: the toolchain wrote past the bind mount "+
			"(BUN_INSTALL_CACHE_DIR did not take)", bunCache)
	}
}
