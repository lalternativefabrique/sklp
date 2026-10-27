package agentdetect

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/skillgen"
)

func TestDetect_RepoMarkerRanksAbovePath(t *testing.T) {
	dir := t.TempDir()
	// A repo marker for Cursor; no binaries are guaranteed in PATH.
	if err := os.MkdirAll(filepath.Join(dir, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := Detect(dir)
	if len(got) == 0 {
		t.Fatal("expected at least the Cursor detection")
	}
	// The repo-marker detection must have strength 2 and ViaRepo set.
	var found bool
	for _, d := range got {
		if d.Agent.Target == skillgen.TargetCursor {
			found = true
			if !d.ViaRepo || d.Strength != 2 {
				t.Errorf("cursor detection should be ViaRepo strength 2, got %+v", d)
			}
		}
	}
	if !found {
		t.Error("cursor not detected from .cursor marker")
	}
	// Results are sorted strongest-first.
	for i := 1; i < len(got); i++ {
		if got[i-1].Strength < got[i].Strength {
			t.Errorf("detections not sorted by strength desc: %v", got)
		}
	}
}

func TestDetect_NothingFound(t *testing.T) {
	dir := t.TempDir()
	// We can't guarantee PATH is empty, so only assert that an empty repo
	// yields no repo-marker (strength-2) detections.
	for _, d := range Detect(dir) {
		if d.ViaRepo {
			t.Errorf("empty repo should yield no repo-marker detection, got %+v", d)
		}
	}
}

func TestDetect_MultipleMarkers(t *testing.T) {
	dir := t.TempDir()
	for _, m := range []string{".claude", ".clinerules"} {
		if err := os.MkdirAll(filepath.Join(dir, m), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got := Detect(dir)
	seen := map[skillgen.Target]bool{}
	for _, d := range got {
		seen[d.Agent.Target] = true
	}
	if !seen[skillgen.TargetClaude] || !seen[skillgen.TargetCline] {
		t.Errorf("expected both Claude and Cline detected, got %v", seen)
	}
}

func TestCatalog_CoversEverySkillgenTarget(t *testing.T) {
	covered := map[skillgen.Target]bool{}
	for _, a := range Catalog() {
		covered[a.Target] = true
	}
	for _, target := range skillgen.AllTargets {
		if !covered[target] {
			t.Errorf("catalog is missing an entry for skillgen target %q", target)
		}
	}
}
