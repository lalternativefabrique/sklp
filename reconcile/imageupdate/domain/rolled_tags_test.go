package domain

import (
	"strings"
	"testing"
	"time"
)

func TestRolledTagsRoundTrip(t *testing.T) {
	j := ParseRolledTags("")
	if !j.Set("altfab-core", "20260902-0459-abc") {
		t.Fatal("first Set reported no change")
	}
	j.Set("altfab-web", "20260902-0459-abc")

	out := j.Render(time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC))
	back := ParseRolledTags(out)
	if back.Tags["altfab-core"] != "20260902-0459-abc" || len(back.Tags) != 2 {
		t.Fatalf("round trip lost data: %v", back.Tags)
	}
	if !strings.Contains(out, "last updated: 2026-09-02T10:00:00Z") {
		t.Fatalf("missing timestamp:\n%s", out)
	}
}

// A tag already journalled must report no change, or every poll would commit.
func TestRolledTagsSetIsIdempotent(t *testing.T) {
	j := ParseRolledTags("tags:\n  web: \"t1\"\n")
	if j.Set("web", "t1") {
		t.Fatal("same tag reported a change")
	}
	if !j.Set("web", "t2") {
		t.Fatal("new tag reported no change")
	}
}

// Rendering is deterministic apart from the timestamp line, so an unchanged
// set never produces a diff on the tags themselves.
func TestRolledTagsRenderIsOrdered(t *testing.T) {
	j := ParseRolledTags("")
	j.Set("zeta", "t")
	j.Set("alpha", "t")
	out := j.Render(time.Now())
	if strings.Index(out, "alpha") > strings.Index(out, "zeta") {
		t.Fatalf("not sorted:\n%s", out)
	}
}

// A malformed journal must not lose the rest nor crash: it is derived state.
func TestParseRolledTagsSurvivesGarbage(t *testing.T) {
	j := ParseRolledTags("# comment\ngarbage line\ntags:\n  good: \"t1\"\n  broken-no-colon\n")
	if j.Tags["good"] != "t1" || len(j.Tags) != 1 {
		t.Fatalf("got %v", j.Tags)
	}
}
