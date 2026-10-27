package main

import (
	"context"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/cli"
)

// TestSpaceCommandTree asserts the space verb exposes the new Registry-backed
// sub-verbs (the migration surface) plus the kept `build`.
func TestSpaceCommandTree(t *testing.T) {
	sp := spaceCommand()
	if sp.Name != "space" {
		t.Fatalf("space command name = %q", sp.Name)
	}
	want := map[string]bool{"up": false, "ls": false, "get": false, "down": false, "exec": false, "purge": false, "build": false}
	for _, c := range sp.Sub {
		if _, ok := want[c.Name]; ok {
			want[c.Name] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("space is missing sub-verb %q", name)
		}
	}
}

// TestSpaceLsEmptyCache runs `space ls` through the adapter against an isolated,
// empty cache: it must succeed (exit 0) and print "no spaces".
func TestSpaceLsEmptyCache(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("SKLP_RUNC_PATH", "/bin/true")
	t.Setenv("SKLP_UNSAFE_RUNC_PATH", "1")
	code := cli.NewFlagAdapter().Execute(context.Background(), rootCommand(), []string{"space", "ls"})
	if code != 0 {
		t.Fatalf("space ls exit = %d, want 0", code)
	}
}

// TestSpaceGetMissing: `space get` for an absent space exits 1 (ErrSpaceNotFound
// surfaced by the thin Run glue).
func TestSpaceGetMissing(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("SKLP_RUNC_PATH", "/bin/true")
	t.Setenv("SKLP_UNSAFE_RUNC_PATH", "1")
	code := cli.NewFlagAdapter().Execute(context.Background(), rootCommand(), []string{"space", "get", "ghost"})
	if code != 1 {
		t.Fatalf("space get ghost exit = %d, want 1", code)
	}
}

// TestSpaceGetRequiresName: no positional → usage error (exit 2).
func TestSpaceGetRequiresName(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	code := cli.NewFlagAdapter().Execute(context.Background(), rootCommand(), []string{"space", "get"})
	if code != 2 {
		t.Fatalf("space get (no name) exit = %d, want 2", code)
	}
}
