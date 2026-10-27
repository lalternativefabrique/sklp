//go:build linux

package runnerbuild

import (
	"slices"
	"strings"
	"testing"
)

// TestCacheEnvForMounts_RedirectsGoModCache is the regression this exists for:
// golang:* ships GOPATH=/go in its image env, so without an explicit
// GOMODCACHE `go mod download` writes to /go/pkg/mod while the cache is bound
// at /root/go/pkg/mod — the mount silently misses and every build re-downloads
// the module graph.
func TestCacheEnvForMounts_RedirectsGoModCache(t *testing.T) {
	got := cacheEnvForMounts([]CacheMount{
		{Source: "/host/go-mod", Target: "/root/go/pkg/mod"},
	})

	if !slices.Contains(got, "GOMODCACHE=/root/go/pkg/mod") {
		t.Errorf("cacheEnvForMounts = %v, want it to export GOMODCACHE at the mount target", got)
	}
	// GOPATH must stay untouched: it also locates GOBIN and the SDK layout, and
	// repointing it would break tool resolution inside the base image.
	for _, e := range got {
		if strings.HasPrefix(e, "GOPATH=") {
			t.Errorf("cacheEnvForMounts exported %q; it must redirect the cache, not GOPATH", e)
		}
	}
}

// TestCacheEnvForMounts_CoversEveryToolchain proves each mounted cache gets its
// matching variable, so npm/bun do not repeat the GOMODCACHE bug.
func TestCacheEnvForMounts_CoversEveryToolchain(t *testing.T) {
	got := cacheEnvForMounts([]CacheMount{
		{Source: "/host/go-mod", Target: "/root/go/pkg/mod"},
		{Source: "/host/go-build", Target: "/root/.cache/go-build"},
		{Source: "/host/npm", Target: "/root/.npm"},
		{Source: "/host/bun", Target: "/root/.bun/install/cache"},
	})

	for _, want := range []string{
		"GOMODCACHE=/root/go/pkg/mod",
		"GOCACHE=/root/.cache/go-build",
		"npm_config_cache=/root/.npm",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("cacheEnvForMounts = %v, missing %q", got, want)
		}
	}
}

// TestCacheEnvForMounts_NoBunEntry pins the other deliberate omission: bun's
// default cache is $HOME/.bun/install/cache and the step shell forces
// HOME=/root, so the default already resolves to the mount target. An explicit
// export would be noise that implies the mount depends on it.
func TestCacheEnvForMounts_NoBunEntry(t *testing.T) {
	got := cacheEnvForMounts([]CacheMount{
		{Source: "/host/bun", Target: "/root/.bun/install/cache"},
	})

	if len(got) != 0 {
		t.Errorf("cacheEnvForMounts = %v, want no bun export (its HOME-derived default already hits the mount)", got)
	}
}

// TestCacheEnvForMounts_NoPnpmEntry pins the deliberate omission: pnpm keeps its
// store on the same filesystem as the install (for hard-links), so it ignores a
// store pointed at a host bind mount and falls back to /.pnpm-store in the
// throwaway rootfs. Exporting PNPM_HOME here would look like the cache works
// while nothing lands in it — a future "fix" must not re-add it blindly.
func TestCacheEnvForMounts_NoPnpmEntry(t *testing.T) {
	got := cacheEnvForMounts([]CacheMount{
		{Source: "/host/pnpm", Target: "/root/.local/share/pnpm"},
	})

	if len(got) != 0 {
		t.Errorf("cacheEnvForMounts = %v, want no pnpm export (its store cannot live on a bind mount)", got)
	}
}

// TestCacheEnvForMounts_OnlyMountedDirs proves an unmounted cache yields no
// export: pointing a toolchain at a path with no bind mount behind it would
// send writes into the throwaway rootfs instead of a persistent cache.
func TestCacheEnvForMounts_OnlyMountedDirs(t *testing.T) {
	got := cacheEnvForMounts([]CacheMount{
		{Source: "/host/go-mod", Target: "/root/go/pkg/mod"},
	})

	for _, e := range got {
		if strings.HasPrefix(e, "npm_config_cache=") {
			t.Errorf("cacheEnvForMounts exported %q with no matching mount", e)
		}
	}
}

// TestCacheEnvForMounts_NoMounts proves the nested-userns path (CacheMounts
// deliberately cleared) exports nothing rather than pointing at absent dirs.
func TestCacheEnvForMounts_NoMounts(t *testing.T) {
	if got := cacheEnvForMounts(nil); len(got) != 0 {
		t.Errorf("cacheEnvForMounts(nil) = %v, want no exports", got)
	}
}

// TestBuildScript_CacheEnvWinsOverImageEnv proves ordering: the assembled shell
// must export GOMODCACHE AFTER the image's own GOPATH, or the base image's
// layout wins and the cache mount misses.
func TestBuildScript_CacheEnvWinsOverImageEnv(t *testing.T) {
	r := RuncStepRunner{
		ImageEnv:    []string{"GOPATH=/go"},
		CacheMounts: []CacheMount{{Source: "/host/go-mod", Target: "/root/go/pkg/mod"}},
	}

	script := r.buildScript([]string{"go mod download"})

	// The exports are shell-quoted, so match on the variable name only.
	gopath := strings.Index(script, "GOPATH=")
	gomod := strings.Index(script, "GOMODCACHE=")
	if gomod < 0 {
		t.Fatalf("buildScript did not export GOMODCACHE:\n%s", script)
	}
	if gopath >= 0 && gomod < gopath {
		t.Errorf("GOMODCACHE exported before the image's GOPATH; the image env would win:\n%s", script)
	}
}
