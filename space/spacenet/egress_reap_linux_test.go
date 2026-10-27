//go:build linux

package spacenet

import "testing"

// TestOrphanSlirpPIDs drives the pure orphan selector with synthetic processes
// and a stub liveness check: only OUR slirp4netns (marker + trailing tap0) whose
// target pid is dead is selected; live targets and unrelated processes are not.
func TestOrphanSlirpPIDs(t *testing.T) {
	const deadPID, livePID = 4242, 777
	alive := func(pid int) bool { return pid == livePID }

	procs := []procEntry{
		// Orphan: our slirp, target 4242 is dead -> reap.
		{pid: 10, argv: []string{"/x/slirp4netns", "--configure", "--mtu=65520", slirpMarker, "4242", "tap0"}},
		// Live: our slirp, target 777 still alive -> keep.
		{pid: 11, argv: []string{"/x/slirp4netns", "--configure", slirpMarker, "777", "tap0"}},
		// Not ours: lacks the marker flag.
		{pid: 12, argv: []string{"/x/slirp4netns", "--configure", "4242", "tap0"}},
		// Not ours: not slirp4netns at all.
		{pid: 13, argv: []string{"/bin/sleep", "1000"}},
		// Not ours: device is not tap0.
		{pid: 14, argv: []string{"/x/slirp4netns", slirpMarker, "4242", "eth9"}},
		// Garbage argv: too short, ignored.
		{pid: 15, argv: []string{"slirp4netns"}},
	}

	got := orphanSlirpPIDs(procs, alive)
	if len(got) != 1 || got[0] != 10 {
		t.Fatalf("orphanSlirpPIDs = %v, want [10]", got)
	}

	// Sanity: with the dead target now reported alive, nothing is reaped.
	if got := orphanSlirpPIDs(procs, func(int) bool { return true }); len(got) != 0 {
		t.Fatalf("all-alive: orphanSlirpPIDs = %v, want []", got)
	}

	_ = deadPID
}

// TestSlirpTargetPID checks the "<pid> tap0" tail parse.
func TestSlirpTargetPID(t *testing.T) {
	if pid, ok := slirpTargetPID([]string{"slirp4netns", "123", "tap0"}); !ok || pid != 123 {
		t.Fatalf("slirpTargetPID = %d,%v want 123,true", pid, ok)
	}
	if _, ok := slirpTargetPID([]string{"slirp4netns", "notanumber", "tap0"}); ok {
		t.Fatal("slirpTargetPID should reject a non-numeric pid token")
	}
}
