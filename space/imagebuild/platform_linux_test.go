package imagebuild

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/space/platform"
)

func TestWithPlatformArgs(t *testing.T) {
	target := platform.Platform{OS: "linux", Arch: "arm64"}
	got := withPlatformArgs(map[string]string{"TARGETARCH": "custom", "FOO": "bar"}, target)
	want := map[string]string{
		"FOO":            "bar",
		"TARGETARCH":     "custom",
		"TARGETOS":       "linux",
		"TARGETPLATFORM": "linux/arm64",
		"BUILDPLATFORM":  "linux/" + runtime.GOARCH,
		"BUILDOS":        "linux",
		"BUILDARCH":      runtime.GOARCH,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("args = %v", got)
	}
}

func TestCheckEmulationNative(t *testing.T) {
	if err := CheckEmulation(platform.Host()); err != nil {
		t.Fatalf("native platform rejected: %v", err)
	}
	if err := CheckEmulation(platform.Platform{}); err != nil {
		t.Fatalf("zero platform rejected: %v", err)
	}
}

func TestCheckEmulationForeignNamesHandler(t *testing.T) {
	foreign := platform.Platform{OS: "linux", Arch: "arm64"}
	if runtime.GOARCH == "arm64" {
		foreign.Arch = "amd64"
	}
	if _, err := os.Stat(binfmtDir + "/qemu-" + foreign.QemuArch()); err == nil {
		t.Skip("host has a qemu handler for " + foreign.String())
	}
	err := CheckEmulation(foreign)
	if err == nil || !strings.Contains(err.Error(), "qemu-user-static") || !strings.Contains(err.Error(), foreign.String()) {
		t.Fatalf("foreign platform without handler: %v", err)
	}
}
