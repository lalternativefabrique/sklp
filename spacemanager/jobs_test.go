package spacemanager

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// startNewAgent builds and runs an agent, returning it plus teardown handles.
func startNewAgent(t *testing.T, l Launcher, rc readyChecker, desired []Service, opts Options) (*Agent, context.CancelFunc, *sync.WaitGroup) {
	t.Helper()
	a := NewAgent(l, rc, desired, opts)
	cancel, wg := startAgent(t, a)
	return a, cancel, wg
}

// --- D: depends_on gating ---

// TestGate_ServiceWaitsForUpstreamServiceReady: a service depends on another
// service; it must not launch until the upstream is Ready.
func TestGate_ServiceWaitsForUpstreamServiceReady(t *testing.T) {
	l := newCounting()
	rc := newFakeChecker(0)
	rc.set("up-0", "block") // up stays not-ready until released
	desired := []Service{
		{Name: "up", Command: []string{"sh", "-c", "sleep 30"}, Ready: &Probe{TCP: "127.0.0.1:1"}},
		{Name: "down", Command: []string{"sh", "-c", "sleep 30"}, DependsOn: []string{"up"}},
	}
	_, cancel, wg := startNewAgent(t, l, rc, desired, readyOpts(nil))
	defer wg.Wait()
	defer cancel()

	// up launches; down is gated, so it must NOT launch while up is not Ready.
	waitFor(t, 2*time.Second, "upstream launched", func() bool { return l.startCount("up-0") >= 1 })
	time.Sleep(200 * time.Millisecond)
	if l.startCount("down-0") != 0 {
		t.Fatalf("dependent launched before upstream Ready (%d)", l.startCount("down-0"))
	}

	close(rc.release) // up becomes Ready
	waitFor(t, 3*time.Second, "dependent launches after upstream Ready", func() bool {
		return l.startCount("down-0") >= 1
	})
}

// TestGate_ServiceWaitsForUpstreamJobDone: a service depends on a job; it must
// not launch until the job reaches Done (exit 0).
func TestGate_ServiceWaitsForUpstreamJobDone(t *testing.T) {
	l := newCounting()
	// The job blocks ~300ms then exits 0.
	desired := []Service{
		{Name: "migrate", Oneshot: true, Command: []string{"sh", "-c", "sleep 0.3"}},
		{Name: "core", Command: []string{"sh", "-c", "sleep 30"}, DependsOn: []string{"migrate"}},
	}
	a, cancel, wg := startNewAgent(t, l, nil, desired, testOpts())
	defer wg.Wait()
	defer cancel()

	waitFor(t, 2*time.Second, "job launched", func() bool { return l.startCount("migrate-0") >= 1 })
	// While the job is still running, core must stay gated.
	if l.startCount("core-0") != 0 {
		t.Fatalf("dependent launched before job Done (%d)", l.startCount("core-0"))
	}
	waitFor(t, 3*time.Second, "job reached Done", func() bool { return a.State("migrate") == Done })
	waitFor(t, 3*time.Second, "dependent launches after job Done", func() bool {
		return l.startCount("core-0") >= 1
	})
}

// TestGate_JobWaitsForUpstreamService: a job depends on a service; the job must
// not launch until the upstream service is Ready.
func TestGate_JobWaitsForUpstreamService(t *testing.T) {
	l := newCounting()
	rc := newFakeChecker(0)
	rc.set("postgres-0", "block")
	desired := []Service{
		{Name: "postgres", Command: []string{"sh", "-c", "sleep 30"}, Ready: &Probe{TCP: "127.0.0.1:1"}},
		{Name: "migrate", Oneshot: true, Command: []string{"sh", "-c", "exit 0"}, DependsOn: []string{"postgres"}},
	}
	a, cancel, wg := startNewAgent(t, l, rc, desired, readyOpts(nil))
	defer wg.Wait()
	defer cancel()

	waitFor(t, 2*time.Second, "postgres launched", func() bool { return l.startCount("postgres-0") >= 1 })
	time.Sleep(200 * time.Millisecond)
	if l.startCount("migrate-0") != 0 {
		t.Fatalf("job launched before upstream service Ready (%d)", l.startCount("migrate-0"))
	}

	close(rc.release)
	waitFor(t, 3*time.Second, "job runs to Done after upstream Ready", func() bool {
		return a.State("migrate") == Done
	})
}

// TestGate_DependentStaysPendingWhileUpstreamJobFails: an upstream job that
// keeps failing (exit != 0) must hold its dependent Pending (never launched),
// fail-closed.
func TestGate_DependentStaysPendingWhileUpstreamJobFails(t *testing.T) {
	l := newCounting()
	desired := []Service{
		{Name: "migrate", Oneshot: true, Command: []string{"sh", "-c", "exit 1"}},
		{Name: "core", Command: []string{"sh", "-c", "sleep 30"}, DependsOn: []string{"migrate"}},
	}
	a, cancel, wg := startNewAgent(t, l, nil, desired, testOpts())
	defer wg.Wait()
	defer cancel()

	waitFor(t, 2*time.Second, "job attempted", func() bool { return l.startCount("migrate-0") >= 1 })
	// Let the job retry/fail for a while; core must never launch.
	time.Sleep(500 * time.Millisecond)
	if l.startCount("core-0") != 0 {
		t.Fatalf("dependent launched despite failing upstream job (%d)", l.startCount("core-0"))
	}
	if a.AllReady() {
		t.Fatal("AllReady true while an upstream job is failing")
	}
}

// --- E: Done terminal + crash-loop distinction ---

// TestJob_CleanExitReachesDoneTerminal: a job that exits 0 reaches Done.
func TestJob_CleanExitReachesDoneTerminal(t *testing.T) {
	l := newCounting()
	desired := []Service{{Name: "job", Oneshot: true, Command: []string{"sh", "-c", "exit 0"}}}
	a, cancel, wg := startNewAgent(t, l, nil, desired, testOpts())
	defer wg.Wait()
	defer cancel()

	waitFor(t, 3*time.Second, "job reached Done", func() bool { return a.State("job") == Done })
}

// TestJob_CleanExitNotCrashLooped: a Done job is NEVER relaunched.
func TestJob_CleanExitNotCrashLooped(t *testing.T) {
	l := newCounting()
	desired := []Service{{Name: "job", Oneshot: true, Command: []string{"sh", "-c", "exit 0"}}}
	a, cancel, wg := startNewAgent(t, l, nil, desired, testOpts())
	defer wg.Wait()
	defer cancel()

	waitFor(t, 3*time.Second, "job reached Done", func() bool { return a.State("job") == Done })
	// Give the reconcile loop ample time to (wrongly) relaunch.
	time.Sleep(500 * time.Millisecond)
	if got := l.startCount("job-0"); got != 1 {
		t.Fatalf("Done job was relaunched (started %d times, want 1)", got)
	}
}

// TestService_CleanExitStillCrashLoops: a NON-oneshot service that exits 0 must
// still be treated as a crash and relaunched (no regression).
func TestService_CleanExitStillCrashLoops(t *testing.T) {
	l := newCounting()
	desired := []Service{{Name: "svc", Command: []string{"sh", "-c", "exit 0"}}}
	a, cancel, wg := startNewAgent(t, l, nil, desired, testOpts())
	defer wg.Wait()
	defer cancel()

	waitFor(t, 3*time.Second, "service relaunched (crash loop)", func() bool {
		return l.startCount("svc-0") >= 2
	})
	if a.State("svc") == Done {
		t.Fatal("a non-oneshot service must never reach Done")
	}
}

// --- F: Failed retry with backoff + cap ---

// TestJob_FailedRetriesWithBackoff: a job that exits != 0 is retried (with
// backoff) before the cap is reached.
func TestJob_FailedRetriesWithBackoff(t *testing.T) {
	l := newCounting()
	rec := &recorder{}
	desired := []Service{{Name: "job", Oneshot: true, Command: []string{"sh", "-c", "exit 1"}}}
	_, cancel, wg := startNewAgent(t, l, nil, desired, readyOpts(rec))
	defer wg.Wait()
	defer cancel()

	waitFor(t, 3*time.Second, "job retried at least twice", func() bool {
		return l.startCount("job-0") >= 2
	})
	if !rec.saw("job", Running, Failed) {
		t.Fatal("missing Running->Failed transition for a failing job")
	}
}

// TestJob_FailedSurfacedAfterCap: a job that keeps failing stops being
// relaunched after the retry cap and HOLDS Failed (fail-closed), so AllReady
// stays false.
func TestJob_FailedSurfacedAfterCap(t *testing.T) {
	l := newCounting()
	desired := []Service{{Name: "job", Oneshot: true, Command: []string{"sh", "-c", "exit 1"}}}
	a, cancel, wg := startNewAgent(t, l, nil, desired, testOpts())
	defer wg.Wait()
	defer cancel()

	// After the cap (jobMaxRetries) the job must stop relaunching and hold Failed.
	waitFor(t, 5*time.Second, "job surfaced as Failed after cap", func() bool {
		return a.State("job") == Failed
	})
	capped := l.startCount("job-0")
	// Give it time to (wrongly) keep retrying past the cap.
	time.Sleep(600 * time.Millisecond)
	if got := l.startCount("job-0"); got != capped {
		t.Fatalf("job kept retrying past the cap (%d -> %d)", capped, got)
	}
	if a.AllReady() {
		t.Fatal("AllReady true while a job is held Failed")
	}
}

// --- G: AllReady accounting ---

// TestAllReady_ExcludesDoneJob: a Done job is excluded from the "every service
// Ready" accounting — a space of (service Ready + job Done) is AllReady.
func TestAllReady_ExcludesDoneJob(t *testing.T) {
	l := newCounting()
	desired := []Service{
		{Name: "svc", Command: []string{"sh", "-c", "sleep 30"}},
		{Name: "job", Oneshot: true, Command: []string{"sh", "-c", "exit 0"}},
	}
	a, cancel, wg := startNewAgent(t, l, nil, desired, testOpts())
	defer wg.Wait()
	defer cancel()

	waitFor(t, 3*time.Second, "job Done and AllReady", func() bool {
		return a.State("job") == Done && a.AllReady()
	})
}

// TestAllReady_BlockedWhileJobStillRunning: while a job is not yet Done, the
// space is NOT AllReady even if every service is Ready.
func TestAllReady_BlockedWhileJobStillRunning(t *testing.T) {
	l := newCounting()
	desired := []Service{
		{Name: "svc", Command: []string{"sh", "-c", "sleep 30"}},
		{Name: "job", Oneshot: true, Command: []string{"sh", "-c", "sleep 30"}}, // never exits in window
	}
	a, cancel, wg := startNewAgent(t, l, nil, desired, testOpts())
	defer wg.Wait()
	defer cancel()

	waitFor(t, 3*time.Second, "service Ready and job launched", func() bool {
		return a.State("svc") == Ready && l.startCount("job-0") >= 1
	})
	time.Sleep(200 * time.Millisecond)
	if a.AllReady() {
		t.Fatal("AllReady true while a job is still running (not Done)")
	}
}

// TestOnAllReady_FiresAfterJobDone: OnAllReady fires once the service is Ready
// AND the job is Done.
func TestOnAllReady_FiresAfterJobDone(t *testing.T) {
	var fired int32
	var mu sync.Mutex
	opts := testOpts()
	opts.OnAllReady = func() { mu.Lock(); fired++; mu.Unlock() }
	l := newCounting()
	desired := []Service{
		{Name: "svc", Command: []string{"sh", "-c", "sleep 30"}},
		{Name: "job", Oneshot: true, Command: []string{"sh", "-c", "sleep 0.2"}},
	}
	a, cancel, wg := startNewAgent(t, l, nil, desired, opts)
	defer wg.Wait()
	defer cancel()

	get := func() int32 { mu.Lock(); defer mu.Unlock(); return fired }
	// Must not fire before the job is Done.
	waitFor(t, 1*time.Second, "service Ready", func() bool { return a.State("svc") == Ready })
	if get() != 0 {
		t.Fatal("OnAllReady fired before the job reached Done")
	}
	waitFor(t, 3*time.Second, "OnAllReady fired after job Done", func() bool {
		return a.State("job") == Done && get() == 1
	})
}

// TestTriggerJob_RerunsCompletedJob: a job that already reached Done is the
// steady state a scheduled run must break out of — TriggerJob puts it back to
// Pending so reconcile launches a second run.
func TestTriggerJob_RerunsCompletedJob(t *testing.T) {
	l := newCounting()
	desired := []Service{
		{Name: "ship", Oneshot: true, Command: []string{"sh", "-c", "exit 0"}},
	}
	a, cancel, wg := startNewAgent(t, l, nil, desired, testOpts())
	defer wg.Wait()
	defer cancel()

	waitFor(t, 3*time.Second, "job reached Done", func() bool { return a.State("ship") == Done })
	if got := l.startCount("ship-0"); got != 1 {
		t.Fatalf("want 1 run before trigger, got %d", got)
	}
	// Left alone, a Done job is never relaunched.
	time.Sleep(200 * time.Millisecond)
	if got := l.startCount("ship-0"); got != 1 {
		t.Fatalf("Done job relaunched without a trigger (%d)", got)
	}

	if !a.TriggerJob("ship") {
		t.Fatal("TriggerJob returned false for a Done job")
	}
	waitFor(t, 3*time.Second, "job ran a second time", func() bool { return l.startCount("ship-0") >= 2 })
	waitFor(t, 3*time.Second, "job reached Done again", func() bool { return a.State("ship") == Done })
}

// TestTriggerJob_ClearsFailureCap: a job held Failed past jobMaxRetries is
// never relaunched by reconcile. A scheduled run must clear that hold,
// otherwise one bad night wedges the schedule permanently.
func TestTriggerJob_ClearsFailureCap(t *testing.T) {
	l := newCounting()
	desired := []Service{
		{Name: "ship", Oneshot: true, Command: []string{"sh", "-c", "exit 1"}},
	}
	a, cancel, wg := startNewAgent(t, l, nil, desired, testOpts())
	defer wg.Wait()
	defer cancel()

	waitFor(t, 10*time.Second, "job held Failed after the retry cap", func() bool {
		return a.State("ship") == Failed && l.startCount("ship-0") >= jobMaxRetries
	})
	held := l.startCount("ship-0")
	time.Sleep(300 * time.Millisecond)
	if got := l.startCount("ship-0"); got != held {
		t.Fatalf("held job relaunched on its own (%d -> %d)", held, got)
	}

	if !a.TriggerJob("ship") {
		t.Fatal("TriggerJob returned false for a held-Failed job")
	}
	waitFor(t, 5*time.Second, "held job runs again after trigger", func() bool {
		return l.startCount("ship-0") > held
	})
}

// TestTriggerJob_RejectsRunningAndNonJob: a run in flight must not be stacked,
// and a long-lived service is not a job.
func TestTriggerJob_RejectsRunningAndNonJob(t *testing.T) {
	l := newCounting()
	desired := []Service{
		{Name: "slow", Oneshot: true, Command: []string{"sh", "-c", "sleep 30"}},
		{Name: "web", Command: []string{"sh", "-c", "sleep 30"}},
	}
	a, cancel, wg := startNewAgent(t, l, nil, desired, testOpts())
	defer wg.Wait()
	defer cancel()

	waitFor(t, 2*time.Second, "job running", func() bool { return a.State("slow") == Running })
	if a.TriggerJob("slow") {
		t.Fatal("TriggerJob stacked a run on a job still in flight")
	}
	if a.TriggerJob("web") {
		t.Fatal("TriggerJob accepted a long-lived service")
	}
	if a.TriggerJob("nope") {
		t.Fatal("TriggerJob accepted an unknown name")
	}
}

// TestRestartService_LeavesCompletedJobDone: a restart bounces live processes,
// and a completed job has none. Before the guard existed, RestartService moved
// the instance off Done into Restarting, which slipped past reconcile's Done
// check and re-ran the job as a side effect of an operator restart — without
// resetting the failure counter TriggerJob resets. Re-running a job is the run
// route's job, so a restart must leave a Done job exactly as it found it.
func TestRestartService_LeavesCompletedJobDone(t *testing.T) {
	l := newCounting()
	desired := []Service{
		{Name: "ship", Oneshot: true, Command: []string{"sh", "-c", "exit 0"}},
	}
	a, cancel, wg := startNewAgent(t, l, nil, desired, testOpts())
	defer wg.Wait()
	defer cancel()

	waitFor(t, 3*time.Second, "job reached Done", func() bool { return a.State("ship") == Done })
	if !a.RestartService("ship") {
		t.Fatal("RestartService returned false for a known job")
	}
	time.Sleep(300 * time.Millisecond)
	if got := l.startCount("ship-0"); got != 1 {
		t.Fatalf("restart re-ran a completed job (%d runs)", got)
	}
	if st := a.State("ship"); st != Done {
		t.Fatalf("restart moved a completed job off Done: %v", st)
	}
}

// TestTriggerJobErr_Reasons: the control API answers 404/400/409 from these
// three errors, so collapsing them into one negative would tell an operator a
// job is missing when a run is merely already in flight.
func TestTriggerJobErr_Reasons(t *testing.T) {
	l := newCounting()
	desired := []Service{
		{Name: "web", Command: []string{"sh", "-c", "sleep 30"}},
		{Name: "slow", Oneshot: true, Command: []string{"sh", "-c", "sleep 30"}},
	}
	a, cancel, wg := startNewAgent(t, l, nil, desired, testOpts())
	defer wg.Wait()
	defer cancel()

	waitFor(t, 3*time.Second, "job running", func() bool { return a.State("slow") == Running })

	if err := a.TriggerJobErr("nope"); !errors.Is(err, ErrJobUnknown) {
		t.Fatalf("unknown service: want ErrJobUnknown, got %v", err)
	}
	if err := a.TriggerJobErr("web"); !errors.Is(err, ErrJobNotOneshot) {
		t.Fatalf("long-lived service: want ErrJobNotOneshot, got %v", err)
	}
	if err := a.TriggerJobErr("slow"); !errors.Is(err, ErrJobInFlight) {
		t.Fatalf("run in flight: want ErrJobInFlight, got %v", err)
	}
}

// TestReconfigure_JobRunsNewImageInsteadOfRolling: an image watch pushing a new
// tag onto a job must RUN that job again — a migration ships to be applied.
// Rolling it like a long-lived service spawns a parallel generation that waits
// for a cutover on Done, which never comes once the run is held Failed: the
// service then stays `rolling` forever, and every later trigger lands on the
// old generation reconcile no longer launches.
func TestReconfigure_JobRunsNewImageInsteadOfRolling(t *testing.T) {
	l := newCounting()
	desired := []Service{
		{Name: "migrate", Oneshot: true, Image: "reg/migrate:v1", Command: []string{"sh", "-c", "exit 0"}},
	}
	a, cancel, wg := startNewAgent(t, l, nil, desired, testOpts())
	defer wg.Wait()
	defer cancel()

	waitFor(t, 3*time.Second, "first run reached Done", func() bool { return a.State("migrate") == Done })

	a.Reconfigure(Service{Name: "migrate", Oneshot: true, Image: "reg/migrate:v2", Command: []string{"sh", "-c", "exit 0"}})

	waitFor(t, 3*time.Second, "the new image ran", func() bool { return l.startCount("migrate-0") >= 2 })
	waitFor(t, 3*time.Second, "the new run reached Done", func() bool { return a.State("migrate") == Done })

	a.mu.Lock()
	rolling := a.services["migrate"].rolling
	a.mu.Unlock()
	if rolling {
		t.Fatal("job left in a rolling update; a later trigger would target a generation reconcile never launches")
	}

	if !a.TriggerJob("migrate") {
		t.Fatal("TriggerJob returned false after the image change")
	}
	waitFor(t, 3*time.Second, "job still re-runnable after the image change", func() bool {
		return l.startCount("migrate-0") >= 3
	})
}

// TestJobRuns_KeepsEveryRunIncludingIdenticalOutput: the point of the history
// is telling a run that just happened from the one before it. Two runs of an
// idempotent migration print the same "no change", and the container log holds
// only the last one (it is reopened O_TRUNC on every launch), so each execution
// must be its own record with its own clock.
func TestJobRuns_KeepsEveryRunIncludingIdenticalOutput(t *testing.T) {
	l := newCounting()
	desired := []Service{
		{Name: "migrate", Oneshot: true, Command: []string{"sh", "-c", "exit 0"}},
	}
	a, cancel, wg := startNewAgent(t, l, nil, desired, testOpts())
	defer wg.Wait()
	defer cancel()

	waitFor(t, 3*time.Second, "first run done", func() bool { return a.State("migrate") == Done })
	waitFor(t, 3*time.Second, "first run recorded", func() bool { return len(a.JobRuns("migrate")) == 1 })

	if !a.TriggerJob("migrate") {
		t.Fatal("TriggerJob returned false for a Done job")
	}
	waitFor(t, 3*time.Second, "second run recorded", func() bool { return len(a.JobRuns("migrate")) == 2 })

	runs := a.JobRuns("migrate")
	if runs[0].ID == runs[1].ID {
		t.Fatalf("both runs share id %q; they cannot be told apart", runs[0].ID)
	}
	if !runs[1].StartedAt.After(runs[0].StartedAt) {
		t.Fatalf("second run did not start after the first (%v vs %v)", runs[1].StartedAt, runs[0].StartedAt)
	}
	for i, r := range runs {
		if r.FinishedAt.Before(r.StartedAt) {
			t.Fatalf("run %d finished before it started", i)
		}
		if r.ExitCode != 0 || r.Failed {
			t.Fatalf("run %d reports exit %d failed=%v, want a clean run", i, r.ExitCode, r.Failed)
		}
	}
	if runs[1].Trigger != "triggered" {
		t.Fatalf("second run trigger = %q, want the operator-initiated one", runs[1].Trigger)
	}
}

// TestJobRuns_RecordsAFailedRun: a failing job must leave a record carrying its
// exit code, or the history only ever explains the runs that went well.
func TestJobRuns_RecordsAFailedRun(t *testing.T) {
	l := newCounting()
	desired := []Service{
		{Name: "migrate", Oneshot: true, Command: []string{"sh", "-c", "exit 3"}},
	}
	a, cancel, wg := startNewAgent(t, l, nil, desired, testOpts())
	defer wg.Wait()
	defer cancel()

	waitFor(t, 5*time.Second, "a failed run was recorded", func() bool { return len(a.JobRuns("migrate")) >= 1 })
	r := a.JobRuns("migrate")[0]
	if !r.Failed || r.ExitCode != 3 {
		t.Fatalf("run reports exit %d failed=%v, want exit 3 failed", r.ExitCode, r.Failed)
	}
}
