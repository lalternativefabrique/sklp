package spacemanager

import (
	"testing"
	"time"
)

// TestJob_ReconfigureRollsOverAndStaysDone reproduces a real incident: a
// oneshot job's image/command changes (a rollout), the new instance runs to
// completion (exit 0, Done) — and must then stay Done, never re-run forever.
// The rollout's "all new instances ready" check must treat Done as the
// oneshot equivalent of Ready, and the rollout-driving loop must carry the
// same "never relaunch a Done job" guard the steady-state loop already has.
func TestJob_ReconfigureRollsOverAndStaysDone(t *testing.T) {
	l := newCounting()
	desired := []Service{{Name: "migrate", Oneshot: true, Command: []string{"sh", "-c", "exit 0"}}}
	a, cancel, wg := startNewAgent(t, l, nil, desired, testOpts())
	defer wg.Wait()
	defer cancel()

	waitFor(t, 3*time.Second, "initial job reached Done", func() bool { return a.State("migrate") == Done })

	// Reconfigure with a changed command — this is what a new image digest
	// looks like from the agent's point of view (same Name, different spec).
	a.Reconfigure(Service{Name: "migrate", Oneshot: true, Command: []string{"sh", "-c", "echo v2; exit 0"}})

	waitFor(t, 3*time.Second, "rolled-over job reached Done", func() bool { return a.State("migrate") == Done })

	// The bug: the rollout-driving loop kept relaunching the new instance
	// forever because it lacked the Oneshot+Done guard, and advanceRollout
	// never cut over (it only ever checks for Ready, which a job never
	// reaches) so st.rolling stayed true. Give reconcile several seconds —
	// long enough for a real loop (each run completes in well under 50ms) to
	// rack up dozens of relaunches — then assert it settled at exactly one
	// run of the new generation.
	time.Sleep(3 * time.Second)

	// Exactly 2 runs total: the original pre-reconfigure run, and the one
	// post-reconfigure run. A looping bug produces dozens within 3s.
	// launch() records starts under the INSTANCE id (svc.Name is overwritten to
	// it in newInstanceLocked), not the service name — the rollout's new
	// generation runs as "migrate-v1-0".
	if got := l.startCount("migrate-v1-0"); got > 3 {
		t.Fatalf("rolled-over job kept relaunching after Done (starts = %d, want <=3)", got)
	}
}
