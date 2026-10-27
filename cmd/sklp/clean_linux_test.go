package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCleanNeedsConfirm(t *testing.T) {
	cases := []struct {
		name string
		opts cleanOpts
		want bool
	}{
		{"default run is rebuildable only", cleanOpts{}, false},
		{"images stay rebuildable", cleanOpts{images: true}, false},
		{"deep destroys spaces and their data", cleanOpts{deep: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.opts.needsConfirm(); got != tc.want {
				t.Errorf("needsConfirm() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCleanCacheLabels(t *testing.T) {
	base := strings.Join(cleanCacheLabels(cleanOpts{}), ", ")
	if strings.Contains(base, "images") {
		t.Errorf("default run must not announce images: %s", base)
	}
	withImages := strings.Join(cleanCacheLabels(cleanOpts{images: true}), ", ")
	if !strings.Contains(withImages, "images") {
		t.Errorf("--images must announce the image cache: %s", withImages)
	}
	// Volumes are never a cache label: they belong to a space and are destroyed
	// with it by --deep, never collected on their own.
	if strings.Contains(withImages, "volumes") {
		t.Errorf("volumes must not be announced as a cache: %s", withImages)
	}
}

// A dry run must render a plan and stop before any removal, so it stays the
// safe way to inspect a reap that SIGKILLs processes of vanished spaces.
func TestRunClean_DryRunDeletesNothing(t *testing.T) {
	var out bytes.Buffer
	if rc := runClean(&out, strings.NewReader(""), cleanOpts{dryRun: true}); rc != 0 {
		t.Fatalf("dry run rc=%d out=%s", rc, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "sklp clean — plan:") {
		t.Errorf("missing plan header: %s", got)
	}
	if !strings.Contains(got, "Nothing deleted (--dry-run)") {
		t.Errorf("missing dry-run notice: %s", got)
	}
	if strings.Contains(got, "cleaned.") {
		t.Errorf("dry run must not report a completed clean: %s", got)
	}
}

// An empty answer at the prompt aborts: a bare Enter must never destroy volumes.
func TestRunClean_ConfirmDefaultsToAbort(t *testing.T) {
	var out bytes.Buffer
	if rc := runClean(&out, strings.NewReader("\n"), cleanOpts{deep: true}); rc != 0 {
		t.Fatalf("aborted run rc=%d out=%s", rc, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "aborted.") {
		t.Errorf("expected abort on empty answer: %s", got)
	}
	if strings.Contains(got, "cleaned.") {
		t.Errorf("abort must not proceed to the clean: %s", got)
	}
}

// The plan's liveness test must stay identical to runcRegistry.Purge's: a space
// whose only container is stopped IS purgeable. A container-count check would
// omit it from the plan and then delete it anyway — the exact mismatch that let
// a 5-space plan tear down 6.
func TestSpaceLivenessMatchesPurgeCriterion(t *testing.T) {
	cases := []struct {
		status string
		live   bool
	}{
		{"running", true},
		{"created", true},
		{"stopped", false},
		{"paused", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			if got := statusIsLive(tc.status); got != tc.live {
				t.Errorf("statusIsLive(%q) = %v, want %v", tc.status, got, tc.live)
			}
		})
	}
}
