package space

import (
	"path/filepath"
	"testing"
)

func TestCacheRoot_HonoursXDGCacheHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)

	got, err := cacheRoot()
	if err != nil {
		t.Fatalf("cacheRoot: %v", err)
	}
	want := filepath.Join(dir, "sklp", "embedded-runtime")
	if got != want {
		t.Errorf("cacheRoot = %q, want %q", got, want)
	}
}

func TestCacheRoot_FallsBackToHomeCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", home)

	got, err := cacheRoot()
	if err != nil {
		t.Fatalf("cacheRoot: %v", err)
	}
	want := filepath.Join(home, ".cache", "sklp", "embedded-runtime")
	if got != want {
		t.Errorf("cacheRoot = %q, want %q", got, want)
	}
}

// TestCentralLogDir locks the host CI-log path. It MUST equal
// pipeline.CentralLogDir — the CI runner bind-mounts this dir onto where the
// inner `sklp run` writes (pipeline.TestCentralLogDir_RepoSlug locks the other
// end). Change one, change both.
func TestCentralLogDir(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "/cache")
	cases := map[string]string{
		"/home/u/code/projects/acme":     "/cache/sklp/logs/acme",
		"/home/u/code/projects/skalpai/": "/cache/sklp/logs/skalpai",
		"":                               "/cache/sklp/logs/unknown",
	}
	for in, want := range cases {
		if got := centralLogDir(in); got != want {
			t.Errorf("centralLogDir(%q) = %q, want %q", in, got, want)
		}
	}
}
