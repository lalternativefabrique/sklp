//go:build linux

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/pipeline"
)

// The cockpit keys the log seam by SPACE name, which is only ever a task name
// for CI spaces (ci-<hash>). A dev space must still tail its repo's newest run
// instead of reporting "no log" for a repo full of them.
func TestStreamCILogs_FallsBackToRepoLatest(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	repo := t.TempDir()

	dir := pipeline.CentralLogDir(repo)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A run of task "ci-sklp" — nothing named after the dev space.
	want := filepath.Join(dir, "ci-ci-sklp-20260803-101112.log")
	if err := os.WriteFile(want, []byte("hello from the run\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var lines []string
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()
	got, err := streamCILogs(ctx, repo, "sklp-dev-c4bfa7c16af2", func(l string) { lines = append(lines, l) })
	if err != nil {
		t.Fatalf("streamCILogs: %v", err)
	}
	if got != want {
		t.Errorf("tailed %q, want the repo's newest run %q", got, want)
	}
	var body string
	for _, l := range lines {
		body += l
	}
	if !strings.Contains(body, "hello from the run") {
		t.Errorf("log body was not streamed, got %v", lines)
	}
}

// An empty repo has genuinely nothing to tail: the error must say so and point
// at the fix, not blame a task name the user never typed.
func TestStreamCILogs_EmptyRepoErrorNamesTheFix(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	repo := t.TempDir()

	_, err := streamCILogs(context.Background(), repo, "sklp-dev-x", func(string) {})
	if err == nil {
		t.Fatal("expected an error for a repo with no runs")
	}
	if !strings.Contains(err.Error(), "sklp run") {
		t.Errorf("error must point at the fix, got: %v", err)
	}
}
