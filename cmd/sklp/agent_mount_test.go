package main

import (
	"testing"

	"github.com/lalternativefabrique/sklp/space"
)

func TestParseSpaceMountsReadsHostAndSpacePaths(t *testing.T) {
	dir := t.TempDir()
	got, err := parseSpaceMounts([]string{dir + ":/workspace", "/dev/null:/workspace/.env:ro"})
	if err != nil {
		t.Fatal(err)
	}
	want := []space.DevMount{
		{Source: dir, Destination: "/workspace"},
		{Source: "/dev/null", Destination: "/workspace/.env", ReadOnly: true},
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestParseSpaceMountsRefusesWhatWouldSilentlyMountSomethingElse(t *testing.T) {
	for _, v := range []string{
		"relative:/workspace",
		"/tmp:relative",
		"/does/not/exist:/workspace",
		"/tmp:/workspace:rx",
		"/tmp",
	} {
		if _, err := parseSpaceMounts([]string{v}); err == nil {
			t.Errorf("%q was accepted", v)
		}
	}
}
