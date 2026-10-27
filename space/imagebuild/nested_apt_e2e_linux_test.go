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

// TestUnshareStepRunner_E2E_NestedAptInstalls is the regression proof for a
// publish whose runtime stage died in apt with "setgroups 65534 failed" and
// "seteuid 42 failed - Invalid argument". The nested runner maps uid 0 and
// nothing else, so apt's privilege drop to _apt cannot succeed; NestedUserns
// on a developer machine reproduces that mapping exactly.
func TestUnshareStepRunner_E2E_NestedAptInstalls(t *testing.T) {
	skipUnlessRuncE2E(t)

	df, err := parse.Parse(`FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
`)
	if err != nil {
		t.Fatal(err)
	}

	cache := t.TempDir()
	res, err := Build(context.Background(), cache, t.TempDir(), df,
		Options{ContextDir: t.TempDir(), Progress: os.Stderr},
		runnerbuild.UnshareStepRunner{NestedUserns: true, Stdout: os.Stderr, Stderr: os.Stderr})
	if err != nil {
		t.Fatalf("apt inside a nested user namespace: %v", err)
	}
	// The accommodation is the runner's, not the image's: it must not ship.
	if _, err := os.Stat(filepath.Join(res.Rootfs, "etc/apt/apt.conf.d/99sklp-nested-userns")); !os.IsNotExist(err) {
		t.Fatalf("the nested apt config leaked into the produced rootfs (stat err=%v)", err)
	}
}
