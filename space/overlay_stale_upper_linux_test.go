//go:build linux

package space

import (
	"os"
	"path/filepath"
	"testing"
)

// seedUpper builds an overlay scratch that was written over lower, with one
// file in the upper standing in for whatever an image's entrypoint wrote.
func seedUpper(t *testing.T, base, lower string) string {
	t.Helper()
	upper := filepath.Join(base, "upper", "etc")
	if err := os.MkdirAll(upper, 0o755); err != nil {
		t.Fatal(err)
	}
	written := filepath.Join(upper, "default.conf")
	if err := os.WriteFile(written, []byte("listen 8080;"), 0o644); err != nil {
		t.Fatal(err)
	}
	if lower != "" {
		if err := os.WriteFile(filepath.Join(base, lowerMarkerName), []byte(lower), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return written
}

// An upper written over one image shadows the next one file for file. chat's
// ui served an nginx config from two images back through nine rollouts, each
// pulling the right image and then hiding it behind this scratch.
func TestDiscardStaleUpperDropsScratchFromAnotherImage(t *testing.T) {
	base := t.TempDir()
	written := seedUpper(t, base, "/rootfs/oldimagedigest")

	if err := discardStaleUpper(base, "/rootfs/newimagedigest"); err != nil {
		t.Fatalf("discardStaleUpper: %v", err)
	}
	if _, err := os.Stat(written); !os.IsNotExist(err) {
		t.Fatal("the previous image's scratch survived a roll onto a new image")
	}
}

// A crash-restart re-runs the same image, and the upper is where the container
// keeps its runtime state. Dropping it on every launch would turn every
// restart into a cold start.
func TestDiscardStaleUpperKeepsScratchOfTheSameImage(t *testing.T) {
	base := t.TempDir()
	lower := "/rootfs/samedigest"
	written := seedUpper(t, base, lower)

	if err := discardStaleUpper(base, lower); err != nil {
		t.Fatalf("discardStaleUpper: %v", err)
	}
	if _, err := os.Stat(written); err != nil {
		t.Fatalf("the scratch of the running image was dropped: %v", err)
	}
}

// An upper left by a node that predates the marker has unknown provenance.
// Treating it as stale costs one cold start; trusting it reinstates the bug.
func TestDiscardStaleUpperDropsScratchWithNoMarker(t *testing.T) {
	base := t.TempDir()
	written := seedUpper(t, base, "")

	if err := discardStaleUpper(base, "/rootfs/anydigest"); err != nil {
		t.Fatalf("discardStaleUpper: %v", err)
	}
	if _, err := os.Stat(written); !os.IsNotExist(err) {
		t.Fatal("an unmarked scratch was trusted")
	}
}

func TestDiscardStaleUpperOnAFreshService(t *testing.T) {
	base := filepath.Join(t.TempDir(), "never-materialised")
	if err := discardStaleUpper(base, "/rootfs/digest"); err != nil {
		t.Fatalf("a service with no scratch yet must be a no-op, got: %v", err)
	}
}
