package domain

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// RolledTagsPath is the file, next to image-watches.yaml, recording the tag
// each watch last rolled to. It is written by the roll, never read by it:
// image-watches.yaml stays the declaration (what to watch), this file is the
// journal (what actually shipped), so `git log` answers "what was live on
// that day". No deploy reads it (ADR-018): the node keeps a rolled image on
// its own until deploy.yaml changes that service's tag.
const RolledTagsPath = ".sklp/image-watches.lock.yaml"

// WatchesPath is the declaration the journal sits next to.
const WatchesPath = ".sklp/image-watches.yaml"

// RolledTags is the parsed journal: watch name -> rolled tag.
type RolledTags struct {
	Tags map[string]string
}

// ParseRolledTags reads the journal's `tags:` block. An unreadable or absent
// file yields an empty set rather than an error: the journal is derived state,
// and a roll must never be blocked by a malformed one — the next write
// rebuilds it whole.
func ParseRolledTags(raw string) RolledTags {
	out := RolledTags{Tags: map[string]string{}}
	inTags := false
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if trimmed == "tags:" {
			inTags = true
			continue
		}
		if !inTags || !strings.HasPrefix(line, " ") {
			continue
		}
		name, tag, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		name = strings.Trim(strings.TrimSpace(name), `"`)
		tag = strings.Trim(strings.TrimSpace(tag), `"`)
		if name != "" && tag != "" {
			out.Tags[name] = tag
		}
	}
	return out
}

// Render emits the journal, deterministically ordered so an unchanged set
// produces a byte-identical file (no empty commit).
func (r RolledTags) Render(now time.Time) string {
	var b strings.Builder
	b.WriteString("# Tags rolled onto the node by Skalpai's image watches — WRITTEN BY THE\n")
	b.WriteString("# ROLL, not by hand. image-watches.yaml declares what to watch; this\n")
	b.WriteString("# file is a journal of what actually shipped, so `git log` answers what\n")
	b.WriteString("# was live when. No deploy reads it: the node keeps a rolled image on\n")
	b.WriteString("# its own until deploy.yaml changes that service's tag. Editing this\n")
	b.WriteString("# file changes nothing: the next roll overwrites it.\n")
	fmt.Fprintf(&b, "# last updated: %s\n", now.UTC().Format(time.RFC3339))
	b.WriteString("tags:\n")
	names := make([]string, 0, len(r.Tags))
	for name := range r.Tags {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&b, "  %s: %q\n", name, r.Tags[name])
	}
	return b.String()
}

// Set records a watch's rolled tag, reporting whether anything changed — a
// no-op roll (same tag) must not produce a commit.
func (r RolledTags) Set(watchName, tag string) bool {
	if r.Tags[watchName] == tag {
		return false
	}
	r.Tags[watchName] = tag
	return true
}
