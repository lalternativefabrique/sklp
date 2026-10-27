package xdg

import (
	"path/filepath"
	"testing"
)

func TestCacheRoot(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "/tmp/xdgcache")
	if got, want := CacheRoot(), "/tmp/xdgcache/sklp"; got != want {
		t.Fatalf("CacheRoot with XDG_CACHE_HOME = %q, want %q", got, want)
	}

	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "/home/tester")
	if got, want := CacheRoot(), "/home/tester/.cache/sklp"; got != want {
		t.Fatalf("CacheRoot fallback = %q, want %q", got, want)
	}
}

func TestRuntimeRunRoot(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if got, want := RuntimeRunRoot(), "/run/user/1000/sklp"; got != want {
		t.Fatalf("RuntimeRunRoot with XDG_RUNTIME_DIR = %q, want %q", got, want)
	}

	// No runtime dir → cache fallback under run/sklp.
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("XDG_CACHE_HOME", "/tmp/xdgcache")
	if got, want := RuntimeRunRoot(), filepath.Join("/tmp/xdgcache/sklp", "run", "sklp"); got != want {
		t.Fatalf("RuntimeRunRoot fallback = %q, want %q", got, want)
	}
}
