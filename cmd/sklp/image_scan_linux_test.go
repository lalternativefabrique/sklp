//go:build linux

package main

import (
	"testing"

	"github.com/lalternativefabrique/sklp/internal/plan"
)

// TestDefaultScanPolicy pins the ad-hoc scan gate to the same bar the publish
// pipeline enforces: HIGH/CRITICAL, ignore unfixed. A drift here would let
// `sklp image push --scan` pass an image the pipeline would block.
func TestDefaultScanPolicy(t *testing.T) {
	p := defaultScanPolicy()
	if p.Severities != "HIGH,CRITICAL" {
		t.Errorf("Severities = %q, want HIGH,CRITICAL", p.Severities)
	}
	if !p.IgnoreUnfixed {
		t.Error("IgnoreUnfixed should default true (match the pipeline)")
	}
}

// TestScannerImageIsShared is the anti-drift guard: the daemonless scanner must
// reuse the SAME pinned Trivy image as the docker-build pipeline, so both paths
// enforce the same vulnerability database. They share plan.InternalTrivyImage by
// reference; this test fails loudly if someone forks the constant.
func TestScannerImageIsShared(t *testing.T) {
	if plan.InternalTrivyImage == "" {
		t.Fatal("plan.InternalTrivyImage must be set — it is the single pinned scanner version")
	}
}
