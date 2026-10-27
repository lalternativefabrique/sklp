//go:build linux

package imagebuild

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	parse "github.com/lalternativefabrique/sklp/pipeline/imagebuild"
	"github.com/lalternativefabrique/sklp/space/runnerbuild"
)

// TestUnshareStepRunner_E2E_NestedApkResolves covers the Alpine runtime stage
// that failed on the CI node with "DNS: transient error": the image ships no
// /etc/resolv.conf, and nested the runner could not bind one in, so musl had
// nothing but 127.0.0.1. The copied resolver must work for apk and must not
// remain in the produced rootfs.
func TestUnshareStepRunner_E2E_NestedApkResolves(t *testing.T) {
	skipUnlessRuncE2E(t)

	df, err := parse.Parse(`FROM alpine:3.21
RUN apk add --no-cache ca-certificates
`)
	if err != nil {
		t.Fatal(err)
	}

	res, err := Build(context.Background(), t.TempDir(), t.TempDir(), df,
		Options{ContextDir: t.TempDir(), Progress: os.Stderr},
		runnerbuild.UnshareStepRunner{NestedUserns: true, Stdout: os.Stderr, Stderr: os.Stderr})
	if err != nil {
		t.Fatalf("apk inside a nested user namespace: %v", err)
	}
	for _, leftover := range []string{"etc/resolv.conf", "etc/resolv.conf.sklp-orig"} {
		if _, err := os.Stat(filepath.Join(res.Rootfs, leftover)); !os.IsNotExist(err) {
			t.Fatalf("%s leaked into the produced rootfs (stat err=%v)", leftover, err)
		}
	}
}
