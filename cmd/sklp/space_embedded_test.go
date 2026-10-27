//go:build linux

package main

import (
	"strings"
	"testing"
)

// Data deletion is never silent: a teardown names the volumes it destroyed,
// since nothing will bring them back.
func TestDeletedVolumesSuffix(t *testing.T) {
	if got := deletedVolumesSuffix(nil); got != "" {
		t.Errorf("no volumes must add no suffix, got %q", got)
	}
	got := deletedVolumesSuffix([]string{"pgdata", "natsdata"})
	if !strings.Contains(got, "2 volume(s)") || !strings.Contains(got, "pgdata") || !strings.Contains(got, "natsdata") {
		t.Errorf("suffix must count and name the volumes, got %q", got)
	}
}

// Volumes are reported space-qualified: two spaces may each hold a "pgdata" and
// the operator must see which one went.
func TestQualifiedVolumesOnlyCoversReapedSpaces(t *testing.T) {
	bySpace := map[string][]string{
		"a": {"pgdata"},
		"b": {"pgdata"},
		"c": {"kept"},
	}
	got := qualifiedVolumes(bySpace, []string{"a", "b"})
	want := []string{"a/pgdata", "b/pgdata"}
	if len(got) != len(want) {
		t.Fatalf("qualifiedVolumes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("qualifiedVolumes = %v, want %v", got, want)
		}
	}
}

// A mistyped subcommand must never be taken for a space name: `sklp space dow x`
// otherwise pulls a base image and builds a rootfs named "dow" before anything
// looks wrong, and the user has to Ctrl-C out of it.
func TestNearestSpaceSubcommand(t *testing.T) {
	cases := []struct {
		word string
		want string
	}{
		{"dow", "down"}, // the typo that triggered this
		{"dwon", "down"},
		{"purg", "purge"},
		{"exce", "exec"},
		{"bui", "build"}, // prefix of 3+
		{"down", ""},     // exact: routed by the framework
		{"up", ""},
		{"mydev", ""}, // a real space name must still work
		{"garden", ""},
		{"x", ""}, // too short to guess anything
	}
	for _, tc := range cases {
		t.Run(tc.word, func(t *testing.T) {
			if got := nearestSpaceSubcommand(tc.word); got != tc.want {
				t.Errorf("nearestSpaceSubcommand(%q) = %q, want %q", tc.word, got, tc.want)
			}
		})
	}
}

func TestEditDistanceAtMostOne(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"down", "down", true},
		{"dow", "down", true},   // deletion
		{"downn", "down", true}, // insertion
		{"dowm", "down", true},  // substitution
		{"dwon", "down", false}, // transposition is 2 edits
		{"up", "down", false},
		{"", "down", false},
	}
	for _, tc := range cases {
		if got := editDistanceAtMostOne(tc.a, tc.b); got != tc.want {
			t.Errorf("editDistanceAtMostOne(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
