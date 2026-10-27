package runnerbuild

import (
	"strings"
	"testing"
)

// The tools dir must not shadow the base image's own toolchain. A golang base
// ships go at /usr/local/go/bin; with /opt/sklp/tools/bin first, the build
// picked the runner's trimmed `go` and died with "cannot find GOROOT
// directory" — a failure that only appears building an application image.
func TestToolsPathDoesNotShadowTheImageToolchain(t *testing.T) {
	// Both runners build their own export line, and fixing only one leaves the
	// other broken — which is exactly what happened.
	for name, exports := range map[string]string{
		"unshare": defaultStepExports,
		"runc":    runcStepExports,
	} {
		t.Run(name, func(t *testing.T) {
			toolsAt := strings.Index(exports, "/opt/sklp/tools/bin")
			imageAt := strings.Index(exports, "/usr/local/bin")

			if toolsAt < 0 {
				t.Fatal("the tools dir must stay on PATH: a runner-image build reaches its baked tools through it")
			}
			if imageAt < 0 {
				t.Fatal("the image's own directories must stay on PATH")
			}
			if toolsAt < imageAt {
				t.Fatalf("the tools dir precedes the image directories; it must come last:\n%s", exports)
			}
		})
	}
}
