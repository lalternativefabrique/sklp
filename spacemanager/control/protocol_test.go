package control

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestSocketPathXDGRuntime: with $XDG_RUNTIME_DIR set, the socket lives under
// <runtime>/sklp/<space>.sock.
func TestSocketPathXDGRuntime(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	got := SocketPath("production")
	want := filepath.Join("/run/user/1000", "sklp", "production.sock")
	if got != want {
		t.Fatalf("SocketPath = %q, want %q", got, want)
	}
}

// TestSocketPathFallback: without $XDG_RUNTIME_DIR, the socket falls back under
// the cache root's run/sklp/ dir, honouring $XDG_CACHE_HOME.
func TestSocketPathFallback(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("XDG_CACHE_HOME", "/tmp/mycache")
	got := SocketPath("preview-7")
	want := filepath.Join("/tmp/mycache", "sklp", "run", "sklp", "preview-7.sock")
	if got != want {
		t.Fatalf("SocketPath = %q, want %q", got, want)
	}
}

// TestSocketPathShape: the path always ends with "<space>.sock".
func TestSocketPathShape(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if got := SocketPath("api"); !strings.HasSuffix(got, "/api.sock") {
		t.Fatalf("SocketPath = %q, want suffix /api.sock", got)
	}
}
