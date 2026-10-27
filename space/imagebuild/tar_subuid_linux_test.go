//go:build linux

package imagebuild

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestTarRootfsReadsSubuidOwnedDir is the regression guard for the publish
// failures on the CI nodes: apt leaves /var/cache/apt/archives/partial as a
// 0700 dir owned by a mapped subuid (100041), and the layer export must still
// read it. Keying the mapping on os.Getuid()!=0 skipped the subuid range
// whenever the build ran as real root — every CI publish died with
// "tar: ./var/cache/apt/archives/partial: Cannot open: Permission denied".
func TestTarRootfsReadsSubuidOwnedDir(t *testing.T) {
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skip("unshare not available")
	}
	if _, err := os.ReadFile("/proc/self/uid_map"); err != nil {
		t.Skip("no /proc/self/uid_map")
	}

	rootfs := t.TempDir()
	const rel = "var/cache/apt/archives/partial"
	if err := os.MkdirAll(filepath.Join(rootfs, rel), 0o755); err != nil {
		t.Fatal(err)
	}

	// Recreate what a RUN step leaves behind: chown to a uid only the build
	// mapping resolves, then lock the dir to its owner.
	setup := exec.Command("unshare", "--user", "--map-root-user",
		"--map-users=1:100000:65535", "--map-groups=1:100000:65535",
		"--mount", "--pid", "--fork",
		"sh", "-c", "chown 42:0 '"+filepath.Join(rootfs, rel)+"' && chmod 0700 '"+filepath.Join(rootfs, rel)+"'")
	if out, err := setup.CombinedOutput(); err != nil {
		t.Skipf("cannot stage a subuid-owned dir (no usable subuid range?): %v: %s", err, out)
	}

	var buf bytes.Buffer
	if err := tarRootfsInUserns(context.Background(), rootfs, &buf); err != nil {
		t.Fatalf("tar rootfs: %v", err)
	}

	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			t.Fatalf("%s missing from the layer", rel)
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimPrefix(h.Name, "./") == rel+"/" {
			return
		}
	}
}
