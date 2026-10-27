package spacemanager

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestJob_FastExitMultiAgentReapTheft reproduces the node-agent shape: several
// agents (one per space) live in ONE process, each with its own pidfdWatcher —
// each a Wait4(-1) reaper. Whichever reapLoop wins the SIGCHLD race harvests
// ANY agent's child, so with per-watcher reap maps an exit code landed in the
// wrong watcher and the owner's oneshot spun in "running" forever. Concurrent
// fast jobs across several agents make the theft near-certain.
func TestJob_FastExitMultiAgentReapTheft(t *testing.T) {
	const agents, jobs, rounds = 4, 4, 12
	for round := 0; round < rounds; round++ {
		type running struct {
			a      *Agent
			cancel func()
			wg     *sync.WaitGroup
		}
		live := make([]running, 0, agents)
		for i := 0; i < agents; i++ {
			desired := make([]Service, 0, jobs)
			for j := 0; j < jobs; j++ {
				desired = append(desired, Service{
					Name:    fmt.Sprintf("job%d", j),
					Oneshot: true,
					Command: []string{"true"},
				})
			}
			a, cancel, wg := startNewAgent(t, newCounting(), nil, desired, testOpts())
			live = append(live, running{a, cancel, wg})
		}
		deadline := time.Now().Add(10 * time.Second)
		for i, r := range live {
			for j := 0; j < jobs; j++ {
				name := fmt.Sprintf("job%d", j)
				for r.a.State(name) != Done {
					if time.Now().After(deadline) {
						for _, r2 := range live {
							r2.cancel()
							r2.wg.Wait()
						}
						t.Fatalf("round %d: agent %d %s stuck in %v", round, i, name, r.a.State(name))
					}
					time.Sleep(2 * time.Millisecond)
				}
			}
		}
		for _, r := range live {
			r.cancel()
			r.wg.Wait()
		}
	}
}

// TestJob_FastExitNeverStuckRunning reproduces the field bug where a oneshot
// exiting within ~1ms of launch missed its running→done transition at space
// materialization and stayed "running" forever (process gone, status lying).
// Each round materializes a fresh agent with several instant-exit jobs, the
// shape of a real space boot.
func TestJob_FastExitNeverStuckRunning(t *testing.T) {
	const rounds, jobs = 60, 8
	for round := 0; round < rounds; round++ {
		l := newCounting()
		desired := make([]Service, 0, jobs)
		for j := 0; j < jobs; j++ {
			desired = append(desired, Service{
				Name:    fmt.Sprintf("job%d", j),
				Oneshot: true,
				Command: []string{"true"},
			})
		}
		a, cancel, wg := startNewAgent(t, l, nil, desired, testOpts())

		deadline := time.Now().Add(5 * time.Second)
		for j := 0; j < jobs; j++ {
			name := fmt.Sprintf("job%d", j)
			for a.State(name) != Done {
				if time.Now().After(deadline) {
					cancel()
					wg.Wait()
					t.Fatalf("round %d: %s stuck in %v after 5s", round, name, a.State(name))
				}
				time.Sleep(2 * time.Millisecond)
			}
		}
		cancel()
		wg.Wait()
	}
}
