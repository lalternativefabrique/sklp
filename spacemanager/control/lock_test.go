package control

import (
	"strings"
	"testing"
)

// TestSpaceLockExclusive: a 2nd acquisition of the same space lock fails
// while the 1st is held; releasing the 1st lets a fresh acquisition succeed.
func TestSpaceLockExclusive(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	release1, err := AcquireSpaceLock("production")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	if _, err := AcquireSpaceLock("production"); err == nil {
		t.Fatal("second acquire succeeded while first held; want contention error")
	} else if !strings.Contains(err.Error(), "already being supervised") {
		t.Fatalf("contention error = %v, want 'already being supervised'", err)
	}

	release1()

	release2, err := AcquireSpaceLock("production")
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	release2()
}

// TestSpaceLockDistinctSpaces: locks are per-space, so two different
// spaces can be supervised at once.
func TestSpaceLockDistinctSpaces(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	r1, err := AcquireSpaceLock("production")
	if err != nil {
		t.Fatalf("acquire production: %v", err)
	}
	defer r1()
	r2, err := AcquireSpaceLock("staging")
	if err != nil {
		t.Fatalf("acquire staging while production held: %v", err)
	}
	defer r2()
}

// TestSpaceLockReleaseIdempotent: calling release twice is safe (no panic, no
// double-close).
func TestSpaceLockReleaseIdempotent(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	release, err := AcquireSpaceLock("production")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	release()
	release() // must be a no-op
}
