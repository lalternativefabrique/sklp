//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/lalternativefabrique/sklp/pipeline"
)

// tagRE matches the immutable date-sha tag: YYYYMMDD-HHMM-<short-sha>. The
// SHA width is pinned, not open-ended: an open `{7,}` let the same commit tag
// as both -e810cff and -e810cffd5 depending on the clone depth.
var tagRE = regexp.MustCompile(fmt.Sprintf(`^\d{8}-\d{4}-[0-9a-f]{%d}$`, pipeline.ShortSHALen))

// initRepo makes a throwaway git repo with one commit and returns its path so
// autoTag has a HEAD to resolve.
func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
		{"commit", "--allow-empty", "-qm", "seed"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// TestAutoTag_DateShaShape locks the auto tag to the publish_tag date-sha
// format, so an ad-hoc `sklp image push` produces the same immutable shape the
// pipeline does.
func TestAutoTag_DateShaShape(t *testing.T) {
	repo := initRepo(t)
	got, err := autoTag(repo)
	if err != nil {
		t.Fatalf("autoTag: %v", err)
	}
	if !tagRE.MatchString(got) {
		t.Errorf("autoTag = %q, want YYYYMMDD-HHMM-<short-sha>", got)
	}
}

// TestImageCacheRoot_HonoursXDG pins the cache layout to the embedded runtime's
// (<XDG_CACHE_HOME>/sklp/embedded-runtime), so image builds share the same
// rootfs/blob cache as the rest of the runtime instead of a private one.
func TestImageCacheRoot_HonoursXDG(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)
	got, err := imageCacheRoot()
	if err != nil {
		t.Fatalf("imageCacheRoot: %v", err)
	}
	want := filepath.Join(root, "sklp", "embedded-runtime")
	if got != want {
		t.Errorf("imageCacheRoot = %q, want %q", got, want)
	}
}

// TestRunImagePush_RequiresImage is the guard: without --image the command must
// fail (exit 2) rather than build to an empty ref.
func TestRunImagePush_RequiresImage(t *testing.T) {
	// runImagePush writes to os.Stderr; we only assert the exit code here.
	if code := runImagePush(imageParams{dockerfile: "Dockerfile"}); code != 2 {
		t.Errorf("runImagePush without --image = %d, want 2", code)
	}
}

// TestRunImagePush_MissingDockerfile fails cleanly (exit 1) when the Dockerfile
// path does not exist, before any build is attempted.
func TestRunImagePush_MissingDockerfile(t *testing.T) {
	repo := initRepo(t)
	code := runImagePush(imageParams{
		image:      "localhost:5000/x",
		contextDir: repo,
		dockerfile: filepath.Join(repo, "Dockerfile.nope"),
		tag:        "t",
	})
	if code != 1 {
		t.Errorf("runImagePush with missing Dockerfile = %d, want 1", code)
	}
	// sanity: the file really is absent
	if _, err := os.Stat(filepath.Join(repo, "Dockerfile.nope")); !os.IsNotExist(err) {
		t.Fatalf("test setup: Dockerfile.nope should not exist")
	}
}
