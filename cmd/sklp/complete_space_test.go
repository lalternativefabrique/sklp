//go:build linux

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestRunCompleteSpace verifies the `sklp __complete space` subject lists the
// existing space names from the runtime cache (socketless dir scan), sorted, and
// stays silent when the cache is empty. Mirrors deployps_test's XDG_CACHE_HOME
// isolation: it only scans dirs, no runc state needed.
func TestRunCompleteSpace(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)

	spaces := filepath.Join(cache, "sklp", "embedded-runtime", "spaces")
	for _, name := range []string{"web", "api", "db"} {
		if err := os.MkdirAll(filepath.Join(spaces, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A stray regular file must NOT be reported as a space.
	if err := os.WriteFile(filepath.Join(spaces, "not-a-dir"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if code := runCompleteTo(&out, ".", []string{"space"}); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if got, want := out.String(), "api\ndb\nweb\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

// TestRunCompleteSpaceEmpty: a missing cache yields no candidates, exit 0.
func TestRunCompleteSpaceEmpty(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	var out bytes.Buffer
	if code := runCompleteTo(&out, ".", []string{"space"}); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if out.String() != "" {
		t.Fatalf("stdout = %q, want empty", out.String())
	}
}
