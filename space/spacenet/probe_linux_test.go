//go:build linux

package spacenet

import "testing"

// TestSpaceNet_ProbeTCPFromSpaceNetns is the go/no-go for the deploy
// readiness fix: a TCP probe must connect to a real listener FROM the space
// netns, with NO `nc`/`wget` binary involved. The space DNS resolver binds
// TCP on the gateway (10.88.0.1:53) inside the space netns, so it is a real,
// always-present listener to dial — proving ProbeTCP reaches a bound port and
// rejects an unbound one, exactly the signal a service readiness probe needs.
func TestSpaceNet_ProbeTCPFromSpaceNetns(t *testing.T) {
	skipUnlessRootlessNet(t)

	cn, err := NewSpaceNet("probe", "10.88.0.0/24")
	if err != nil {
		t.Fatalf("NewSpaceNet: %v", err)
	}
	defer cn.Close()

	// The resolver listens on the gateway:53 (TCP) inside the space netns.
	if !cn.ProbeTCP("10.88.0.1:53") {
		t.Fatalf("ProbeTCP to the in-space DNS listener FAILED (expected ready)")
	}
	// An unbound port on the gateway must report not-ready, not hang.
	if cn.ProbeTCP("10.88.0.1:5") {
		t.Fatalf("ProbeTCP to an unbound port reported ready (expected not-ready)")
	}
}

// TestSpaceNet_ProbeAfterCloseIsFalse confirms a probe on a torn-down space
// reports not-ready instead of panicking on the dead worker pipe.
func TestSpaceNet_ProbeAfterCloseIsFalse(t *testing.T) {
	skipUnlessRootlessNet(t)

	cn, err := NewSpaceNet("probe-closed", "10.88.0.0/24")
	if err != nil {
		t.Fatalf("NewSpaceNet: %v", err)
	}
	cn.Close()

	if cn.ProbeTCP("10.88.0.1:53") {
		t.Fatalf("ProbeTCP on a closed space reported ready")
	}
	if cn.ProbeHTTP("http://10.88.0.1:53/") {
		t.Fatalf("ProbeHTTP on a closed space reported ready")
	}
}
