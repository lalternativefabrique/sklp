//go:build linux

package imagebuild

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	parse "github.com/lalternativefabrique/sklp/pipeline/imagebuild"
	"github.com/lalternativefabrique/sklp/space/runnerbuild"
)

// TestUnshareStepRunner_E2E_ImageToolchainResolvesGOROOT is the regression
// proof for a publish that died on every Go build with "cannot find GOROOT
// directory: 'go' binary is trimmed and GOROOT is not set".
//
// A release toolchain is built with -trimpath, so it carries no compiled-in
// GOROOT and recovers one by reading /proc/self/exe. Nested inside the OCI
// runner mount(2) cannot succeed — CAP_SYS_ADMIN is absent from the bounding
// set and noNewPrivileges is set — so a strict /proc mount aborts the build
// and a missing /proc breaks the toolchain instead. Neither failure names the
// mount that caused it.
//
// Asserting on `go version` rather than on the exit code is what makes this a
// proof: forcing a GOROOT taken from the first `go` on PATH also produced a
// green mount while pinning the runner's toolchain over the image's, and every
// package then failed with "compile: version does not match go tool version".
func TestUnshareStepRunner_E2E_ImageToolchainResolvesGOROOT(t *testing.T) {
	skipUnlessRuncE2E(t)

	const wantGo = "go1.26.6"

	df, err := parse.Parse(`FROM golang:1.26.6-trixie
RUN go env GOROOT && go version
`)
	if err != nil {
		t.Fatal(err)
	}

	// The runner writes to *os.File, so the RUN output is captured through a
	// pipe: it carries the toolchain's own answer, which is what is asserted.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	var captured strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(&captured, r)
	}()

	cache := t.TempDir()
	_, err = Build(context.Background(), cache, t.TempDir(), df,
		Options{ContextDir: t.TempDir(), Progress: w},
		runnerbuild.UnshareStepRunner{NestedUserns: true, Stdout: w, Stderr: w})
	w.Close()
	<-done
	r.Close()
	if err != nil {
		t.Fatalf("build on a golang base failed: %v\n%s", err, captured.String())
	}

	got := captured.String()
	if !strings.Contains(got, "/usr/local/go") {
		t.Fatalf("the toolchain never resolved its GOROOT:\n%s", got)
	}
	// The image's toolchain must win. A GOROOT belonging to the runner makes
	// `go version` report the runner's version while the image's binary runs.
	if !strings.Contains(got, wantGo) {
		t.Fatalf("go reported a version other than the image's %s; a foreign GOROOT was forced:\n%s",
			wantGo, got)
	}
}
