package spacemanager

import (
	"fmt"
	"log"
	"os"
	"testing"
	"time"
)

// TestRuncJob_FastExitNeverStuckRunning is the production-faithful repro of the
// stuck fast-job bug: real runc launcher (runc run -d + pidfile + subreaper
// reparenting), oneshot jobs exiting as fast as the container boots. Needs
// container privileges, so it only runs when SKLP_RUNC_TEST=1.
func TestRuncJob_FastExitNeverStuckRunning(t *testing.T) {
	if os.Getenv("SKLP_RUNC_TEST") != "1" {
		t.Skip("set SKLP_RUNC_TEST=1 to run the runc integration repro")
	}
	rounds := 10
	if v := os.Getenv("SKLP_RUNC_TEST_ROUNDS"); v != "" {
		fmt.Sscanf(v, "%d", &rounds)
	}
	const jobs = 4
	for round := 0; round < rounds; round++ {
		l, rc, err := NewRuncLauncher("")
		if err != nil {
			t.Fatalf("round %d: launcher: %v", round, err)
		}
		desired := make([]Service, 0, jobs)
		for j := 0; j < jobs; j++ {
			desired = append(desired, Service{
				Name:    fmt.Sprintf("job%d", j),
				Oneshot: true,
				Image:   "docker.io/library/busybox:latest",
				Command: []string{"true"},
			})
		}
		opts := testOpts()
		opts.Logger = log.New(os.Stderr, "[agent] ", log.Ltime|log.Lmicroseconds)
		a, cancel, wg := startNewAgent(t, l, rc, desired, opts)

		deadline := time.Now().Add(90 * time.Second)
		for j := 0; j < jobs; j++ {
			name := fmt.Sprintf("job%d", j)
			for a.State(name) != Done {
				if time.Now().After(deadline) {
					cancel()
					wg.Wait()
					t.Fatalf("round %d: %s stuck in %v", round, name, a.State(name))
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		t.Logf("round %d: all %d jobs Done", round, jobs)
		cancel()
		wg.Wait()
	}
}
