//go:build linux

package plan

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGCOrphanWorkDirsSkipsRecentBuildDirs(t *testing.T) {
	parent := t.TempDir()
	recent := filepath.Join(parent, "build-recent")
	if err := os.Mkdir(recent, 0o755); err != nil {
		t.Fatal(err)
	}

	var removed []string
	oldRemove := removeBuildWorkFn
	removeBuildWorkFn = func(path string) { removed = append(removed, filepath.Base(path)) }
	defer func() { removeBuildWorkFn = oldRemove }()

	gcOrphanWorkDirs(parent, time.Now())
	if len(removed) != 0 {
		t.Fatalf("removed recent dirs: %v", removed)
	}
}

func TestGCOrphanWorkDirsRemovesOldBuildDirsOnly(t *testing.T) {
	parent := t.TempDir()
	oldDir := filepath.Join(parent, "build-old")
	recentDir := filepath.Join(parent, "build-recent")
	otherDir := filepath.Join(parent, "cache")
	for _, dir := range []string{oldDir, recentDir, otherDir} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	oldTime := now.Add(-orphanBuildWorkMinAge - time.Minute)
	if err := os.Chtimes(oldDir, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	var removed []string
	oldRemove := removeBuildWorkFn
	removeBuildWorkFn = func(path string) { removed = append(removed, filepath.Base(path)) }
	defer func() { removeBuildWorkFn = oldRemove }()

	gcOrphanWorkDirs(parent, now)
	if len(removed) != 1 || removed[0] != "build-old" {
		t.Fatalf("removed = %v, want [build-old]", removed)
	}
}
