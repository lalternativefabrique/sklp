//go:build linux

package plan

import (
	"strings"
	"testing"
)

func TestTrivyRootfsCommand_IgnoreUnfixed(t *testing.T) {
	got := trivyRootfsCommand("HIGH,CRITICAL", true, "/scan")
	for _, want := range []string{"trivy rootfs", "--severity HIGH,CRITICAL", "--exit-code 1", "--ignore-unfixed", "/scan"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q\n  got: %s", want, got)
		}
	}
}

func TestTrivyRootfsCommand_NoIgnoreUnfixed(t *testing.T) {
	got := trivyRootfsCommand("HIGH", false, "/scan")
	if strings.Contains(got, "--ignore-unfixed") {
		t.Errorf("--ignore-unfixed must be absent when ignoreUnfixed=false\n  got: %s", got)
	}
}

// The scan must run Trivy from the pinned scanner image, never a host-PATH
// binary: a host-resolved Trivy makes the verdict depend on the machine.
func TestScanUsesPinnedTrivyImage(t *testing.T) {
	if InternalTrivyImage == "" {
		t.Fatal("InternalTrivyImage must be pinned")
	}
	if !strings.Contains(InternalTrivyImage, ":") {
		t.Errorf("InternalTrivyImage must carry an explicit tag, got %q", InternalTrivyImage)
	}
}
