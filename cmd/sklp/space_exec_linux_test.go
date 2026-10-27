//go:build linux

package main

import (
	"reflect"
	"testing"
)

// TestParseSpaceExecArgs locks the docker-style tail parsing: name first, the
// rest is the command verbatim (with or without a leading `--`), and a bare name
// yields no command (so the caller defaults to a shell). This is the fix for the
// silent `space exec dev go test` → sh fallback.
func TestParseSpaceExecArgs(t *testing.T) {
	cases := []struct {
		name          string
		tail          []string
		wantName      string
		wantContainer string
		wantCmd       []string
	}{
		{"name only opens shell", []string{"dev"}, "dev", "", nil},
		{"cmd without dashes", []string{"dev", "go", "test"}, "dev", "", []string{"go", "test"}},
		{"cmd with own dashes", []string{"dev", "go", "test", "-run", "X"}, "dev", "", []string{"go", "test", "-run", "X"}},
		{"explicit -- separator", []string{"dev", "--", "go", "test"}, "dev", "", []string{"go", "test"}},
		{"empty tail is usage error", nil, "", "", nil},
		{"container shell", []string{"dev", "-c", "core"}, "dev", "core", nil},
		{"container with cmd", []string{"dev", "-c", "core", "go", "test"}, "dev", "core", []string{"go", "test"}},
		{"container equals form", []string{"dev", "-c=core"}, "dev", "core", nil},
		{"container after --", []string{"dev", "--", "-c", "core"}, "dev", "core", nil},
		{"container then -- before cmd", []string{"dev", "-c", "core", "--", "go", "test"}, "dev", "core", []string{"go", "test"}},
		// A -c belonging to the COMMAND must stay in the command: only a leading one
		// selects the container, else `space exec dev sh -c 'echo hi'` would lose it.
		{"command's own -c is untouched", []string{"dev", "sh", "-c", "echo hi"}, "dev", "", []string{"sh", "-c", "echo hi"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotName, gotContainer, gotCmd := parseSpaceExecArgs(c.tail)
			if gotName != c.wantName {
				t.Errorf("name = %q, want %q", gotName, c.wantName)
			}
			if gotContainer != c.wantContainer {
				t.Errorf("container = %q, want %q", gotContainer, c.wantContainer)
			}
			// An empty command (len 0) is what the caller checks to default to a
			// shell; nil vs empty-slice is irrelevant, so compare on length then
			// contents.
			if len(gotCmd) != len(c.wantCmd) || (len(gotCmd) > 0 && !reflect.DeepEqual(gotCmd, c.wantCmd)) {
				t.Errorf("cmd = %v, want %v", gotCmd, c.wantCmd)
			}
		})
	}
}
