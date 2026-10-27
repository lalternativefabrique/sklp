package space

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLowerDigest_SharedCacheAndSpaceCopy(t *testing.T) {
	cases := map[string]string{
		"/c/rootfs/abc/bin":             "abc",
		"/c/rootfs/abc":                 "abc",
		"/c/spaces/prod/rootfs/abc":     "abc",
		"/c/spaces/prod/rootfs/abc/usr": "abc",
		"/c/spaces/prod/overlay/x":      "",
		"/elsewhere/rootfs/abc":         "",
	}
	for in, want := range cases {
		if got := lowerDigest(in, "/c/rootfs", "/c/spaces"); got != want {
			t.Errorf("lowerDigest(%q) = %q, want %q", in, got, want)
		}
	}
}

// A space's own copy of an image (child userns) is a reference to it while it
// lives and goes with it when the image is collected: neither may outlive
// the other on disk.
func TestImageGC_CollectsSpaceCopiesWithTheImage(t *testing.T) {
	root := gcFixture(t, map[string][]string{"prod": {"repo/a:live"}}, map[string]string{"repo/a:live": "livedigest"}, []string{"livedigest", "staledigest"})
	age(t, root, "staledigest")
	mustMkFile(t, filepath.Join(root, "spaces", "prod", "rootfs", "livedigest", "bin", "sh"))
	mustMkFile(t, filepath.Join(root, "spaces", "prod", "rootfs", "staledigest", "bin", "sh"))
	mustMkFile(t, filepath.Join(root, "spaces", "old", "rootfs", "gonedigest", "bin", "sh"))

	res, err := imageGCAt(root, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	removed := map[string]bool{}
	for _, r := range res.Removed {
		removed[r.Digest] = true
	}
	if !removed["staledigest"] || !removed["gonedigest"] || removed["livedigest"] {
		t.Fatalf("removed = %v", res.Removed)
	}
	for _, p := range []string{
		filepath.Join(root, "spaces", "prod", "rootfs", "staledigest"),
		filepath.Join(root, "spaces", "old", "rootfs", "gonedigest"),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s still on disk", p)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "spaces", "prod", "rootfs", "livedigest", "bin", "sh")); err != nil {
		t.Fatalf("the live image's space copy must stay: %v", err)
	}
}
