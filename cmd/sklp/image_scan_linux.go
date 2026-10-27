//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lalternativefabrique/sklp/internal/plan"
	"github.com/lalternativefabrique/sklp/space"
	"github.com/lalternativefabrique/sklp/space/runnerbuild"
)

// scanPolicy is the daemonless scan gate for `sklp image push --scan`. It mirrors
// the publish pipeline's defaults (HIGH/CRITICAL, ignore unfixed) so an
// ad-hoc push enforces the same bar as a release.
type scanPolicy struct {
	Severities    string // comma-joined, e.g. "HIGH,CRITICAL"
	IgnoreUnfixed bool
}

func defaultScanPolicy() scanPolicy {
	return scanPolicy{Severities: "HIGH,CRITICAL", IgnoreUnfixed: true}
}

// scanRootfs runs Trivy against the just-built image filesystem WITHOUT Docker:
// it pulls the pinned Trivy image (plan.InternalTrivyImage — the SAME version
// the pipeline uses, no drift) as a rootfs via the embedded puller, then runs
// `trivy rootfs /scan` inside that rootfs through the runc step runner, with the
// image's own rootfs bind-mounted read-write at /scan. Trivy's `--exit-code 1`
// makes a finding fail the step, which the runc runner surfaces as an error —
// the gate that aborts the push.
//
// scanTarget is the extracted rootfs of the image to scan (imagebuild Result
// .Rootfs). cacheRoot is the embedded-runtime cache (shared puller/runc).
func scanRootfs(cacheRoot, scanTarget string, pol scanPolicy) error {
	ctx := context.Background()

	// 1. Pull Trivy as a rootfs (cached across runs by digest).
	trivyRootfs, err := space.PrepareImage(ctx, cacheRoot, plan.InternalTrivyImage)
	if err != nil {
		return fmt.Errorf("pull scanner image %s: %w", plan.InternalTrivyImage, err)
	}

	absTarget, err := filepath.Abs(scanTarget)
	if err != nil {
		return err
	}

	// 2. Build the trivy command. `rootfs` mode scans a directory tree with no
	// daemon and no registry — exactly the local filesystem we just built.
	parts := []string{
		"trivy", "rootfs",
		"--severity", pol.Severities,
		"--exit-code", "1",
		"--no-progress",
		"--scanners", "vuln",
	}
	if pol.IgnoreUnfixed {
		parts = append(parts, "--ignore-unfixed")
	}
	parts = append(parts, "/scan")
	cmd := strings.Join(parts, " ")

	// 3. Run it in the Trivy rootfs with the image filesystem bind-mounted at
	// /scan. A non-zero trivy exit (a finding) makes Exec return an error.
	runner := runnerbuild.RuncStepRunner{
		CacheRoot: cacheRoot,
		Stdout:    os.Stderr, // scan output is diagnostic; keep stdout clean for the push line
		Stderr:    os.Stderr,
		CacheMounts: []runnerbuild.CacheMount{
			{Source: absTarget, Target: "/scan"},
		},
	}
	if err := runner.Exec(ctx, trivyRootfs, []string{cmd}); err != nil {
		return fmt.Errorf("vulnerabilities found (or scan failed): %w", err)
	}
	return nil
}
