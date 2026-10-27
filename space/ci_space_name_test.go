package space

import (
	"path/filepath"
	"strings"
	"testing"
)

// Two worktrees of the same repo running the SAME task must land in disjoint
// spaces — this is the whole point of the per-checkout key. A shared space
// means a shared bridge and shared services (postgres, nats), so one run's
// schema reset lands under the other's feet.
func TestCISpaceName_DistinctPerCheckout(t *testing.T) {
	a := ciSpaceName("ci-acme", "/home/u/code/repo")
	b := ciSpaceName("ci-acme", "/home/u/code/repo-worktrees/feat-x")
	if a == b {
		t.Fatalf("two checkouts share a space name: %q", a)
	}
}

// The same checkout must map to the same space across consecutive runs, so a
// second run reuses the space (and its warm volumes) instead of leaking a new
// one every time.
func TestCISpaceName_StableForSameCheckout(t *testing.T) {
	const repo = "/home/u/code/repo"
	if a, b := ciSpaceName("ci", repo), ciSpaceName("ci", repo); a != b {
		t.Fatalf("unstable space name: %q vs %q", a, b)
	}
}

// A relative path and its absolute form denote ONE checkout and must not yield
// two spaces — otherwise `sklp run -repo .` and `-repo /abs/path` would each
// spin up their own postgres.
func TestCISpaceName_RelativeAndAbsoluteAgree(t *testing.T) {
	abs, err := filepath.Abs(".")
	if err != nil {
		t.Skipf("cannot resolve cwd: %v", err)
	}
	if got, want := ciSpaceName("ci", "."), ciSpaceName("ci", abs); got != want {
		t.Fatalf("relative %q != absolute %q", got, want)
	}
}

// Different tasks stay separate even within one checkout: ci-acme and
// ci-transcript declare different services and must not collide.
func TestCISpaceName_DistinctPerTask(t *testing.T) {
	const repo = "/home/u/code/repo"
	if a, b := ciSpaceName("ci-acme", repo), ciSpaceName("ci-transcript", repo); a == b {
		t.Fatalf("distinct tasks share a space name: %q", a)
	}
}

// The name is embedded in both a filesystem path and a runc container id, so
// it must survive sanitizeSpaceName unchanged — otherwise the digest could be
// mangled and two checkouts could re-collide after sanitisation.
func TestCISpaceName_SafeAsPathAndID(t *testing.T) {
	got := ciSpaceName("ci-acme", "/home/u/My Repo (v2)/wt")
	if got != sanitizeSpaceName(got) {
		t.Fatalf("name not stable under sanitisation: %q -> %q", got, sanitizeSpaceName(got))
	}
	if strings.ContainsAny(got, "/ ()") {
		t.Fatalf("name leaked unsafe characters: %q", got)
	}
}

// An unresolvable repo path must fall back to the plain task name rather than
// inventing a fresh identity per call — a random suffix would leak a new space
// (and its containers) on every run.
func TestCISpaceName_EmptyRepoFallsBackToTask(t *testing.T) {
	if got, want := ciSpaceName("ci-acme", ""), "ci-acme"; got != want {
		t.Fatalf("empty repo path: got %q, want %q", got, want)
	}
}
