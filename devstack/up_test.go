package devstack

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUp_ValidateLaunchesNothing runs Up with Validate=true on a minimal
// dev.yaml and asserts it succeeds, prints the validation summary, and
// drops no run-state file (i.e. it never reached the launch path).
func TestUp_ValidateLaunchesNothing(t *testing.T) {
	repo := t.TempDir()
	taskPath := filepath.Join(repo, "dev.yaml")
	src := []byte(`version: 1
kind: devstack
services:
  api: { run: "echo a" }
`)
	if err := os.WriteFile(taskPath, src, 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errBuf bytes.Buffer
	cfg := DevConfig{
		TaskPath: taskPath,
		Repo:     repo,
		Validate: true,
		Stdout:   &out,
		Stderr:   &errBuf,
	}
	if err := Up(context.Background(), cfg); err != nil {
		t.Fatalf("Up validate: %v (stderr=%q)", err, errBuf.String())
	}
	if !strings.Contains(out.String(), "service(s)") {
		t.Fatalf("expected validation summary on stdout, got %q", out.String())
	}
	// No launch ⇒ no run-state file written.
	if _, err := os.Stat(statePath(repo, "")); !os.IsNotExist(err) {
		t.Fatalf("validate must not write run-state, stat err=%v", err)
	}
}

// TestScope_PerStack asserts the scoping helpers are stack-aware: two
// stacks on the same repo derive distinct scope ids / space names, while
// Stack=="" reproduces the legacy repo-only name (backward compatible).
func TestScope_PerStack(t *testing.T) {
	repo := "/home/dev/skalpai"

	legacy := devScopeID(repo, "")
	a := devScopeID(repo, "core")
	b := devScopeID(repo, "transcript")

	if a == b {
		t.Fatalf("different stacks must not share a scope id (both %q)", a)
	}
	if a == legacy || b == legacy {
		t.Fatalf("per-stack scope id must differ from the legacy id %q (a=%q b=%q)", legacy, a, b)
	}

	// devScopeID(repo, "") must equal the pre-change repo-only hash so
	// existing single-stack runs / down statefiles keep resolving.
	if got, want := devSpaceName(repo, ""), "sklp-dev-"+legacy; got != want {
		t.Fatalf("legacy space name drift: got %q want %q", got, want)
	}
	if devSpaceName(repo, "core") == devSpaceName(repo, "transcript") {
		t.Fatal("per-stack space names must differ")
	}
	if devSpaceName(repo, "core") == devSpaceName(repo, "") {
		t.Fatal("per-stack space name must differ from the legacy name")
	}

	// statefile path follows the same scoping.
	if statePath(repo, "core") == statePath(repo, "transcript") {
		t.Fatal("per-stack statefile paths must differ")
	}
	if statePath(repo, "") == statePath(repo, "core") {
		t.Fatal("legacy statefile must differ from a per-stack one")
	}
}
