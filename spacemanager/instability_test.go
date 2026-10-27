package spacemanager

import (
	"testing"
	"time"
)

func snapshotOf(t *testing.T, a *Agent, service string) ServiceSnapshot {
	t.Helper()
	for _, s := range a.Snapshot() {
		if s.Name == service {
			return s
		}
	}
	t.Fatalf("service %q absent from snapshot", service)
	return ServiceSnapshot{}
}

// firstInstance is the service's single instance, once the agent has created
// it. Snapshots taken before the first launch carry none.
func firstInstance(t *testing.T, a *Agent, service string) (InstanceSnapshot, bool) {
	t.Helper()
	insts := snapshotOf(t, a, service).Instances
	if len(insts) == 0 {
		return InstanceSnapshot{}, false
	}
	return insts[0], true
}

// TestSnapshotReportsRestartsOfACrashLoop is the whole point of the restart
// counters: a service that dies and comes back is Ready again by the time
// anyone polls, so the state alone reads identically to a service that never
// moved. Restarts is what tells the two apart.
func TestSnapshotReportsRestartsOfACrashLoop(t *testing.T) {
	l := newCounting()
	desired := []Service{{Name: "flap", Command: []string{"sh", "-c", "sleep 0.05"}}}
	a := NewAgent(l, nil, desired, testOpts())
	cancel, wg := startAgent(t, a)
	defer wg.Wait()
	defer cancel()

	waitFor(t, 5*time.Second, "flap to restart at least three times", func() bool {
		inst, ok := firstInstance(t, a, "flap")
		return ok && inst.Restarts >= 3
	})

	inst, _ := firstInstance(t, a, "flap")
	if inst.StartedAt.IsZero() {
		t.Fatal("StartedAt is zero: a restarting instance must date its current run")
	}
	if age := time.Since(inst.StartedAt); age > 5*time.Second {
		t.Fatalf("StartedAt is stale (%s old): it must track the CURRENT run, not the first", age)
	}
}

// TestSnapshotReportsBackoffGrowth: the backoff climbs while a service keeps
// dying, so its value is a crash loop stated outright rather than inferred
// from a sequence of reads.
func TestSnapshotReportsBackoffGrowth(t *testing.T) {
	l := newCounting()
	desired := []Service{{Name: "doomed", Command: []string{"sh", "-c", "exit 1"}}}
	a := NewAgent(l, nil, desired, testOpts())
	cancel, wg := startAgent(t, a)
	defer wg.Wait()
	defer cancel()

	waitFor(t, 5*time.Second, "backoff to grow past its base", func() bool {
		inst, ok := firstInstance(t, a, "doomed")
		return ok && inst.Backoff > 50*time.Millisecond
	})
}

// TestSnapshotReportsSteadyServiceAsUnrestarted guards the other direction: a
// service that stays up must report zero restarts, or the signal would flag
// every healthy space.
func TestSnapshotReportsSteadyServiceAsUnrestarted(t *testing.T) {
	l := newCounting()
	desired := []Service{{Name: "steady", Command: []string{"sh", "-c", "sleep 30"}}}
	a := NewAgent(l, nil, desired, testOpts())
	cancel, wg := startAgent(t, a)
	defer wg.Wait()
	defer cancel()

	waitFor(t, 5*time.Second, "steady to be ready", func() bool {
		return snapshotOf(t, a, "steady").State == Ready
	})

	inst, _ := firstInstance(t, a, "steady")
	if inst.Restarts != 0 {
		t.Fatalf("Restarts = %d, want 0 for a service that never died", inst.Restarts)
	}
	if !inst.RestartAt.IsZero() {
		t.Fatalf("RestartAt = %v, want zero when no restart is scheduled", inst.RestartAt)
	}
	if inst.Held {
		t.Fatal("Held is set on a live service")
	}
}

// TestSnapshotReportsHeldJob: a job that exhausts its retry cap is terminal
// and keeps AllReady false forever, yet it carries the same Failed state as a
// job that has only just missed once. Held is what marks it permanent.
func TestSnapshotReportsHeldJob(t *testing.T) {
	l := newCounting()
	desired := []Service{{
		Name:    "migrate",
		Command: []string{"sh", "-c", "exit 1"},
		Oneshot: true,
	}}
	a := NewAgent(l, nil, desired, testOpts())
	cancel, wg := startAgent(t, a)
	defer wg.Wait()
	defer cancel()

	waitFor(t, 10*time.Second, "migrate to be held past its retry cap", func() bool {
		inst, ok := firstInstance(t, a, "migrate")
		return ok && inst.Held
	})

	inst, _ := firstInstance(t, a, "migrate")
	if inst.JobFails < jobMaxRetries {
		t.Fatalf("JobFails = %d, want at least the retry cap %d", inst.JobFails, jobMaxRetries)
	}
	if a.AllReady() {
		t.Fatal("AllReady is true with a held job: it must stay false")
	}
}

// TestRestartServiceDoesNotCountAsInstability: bouncing a service is what an
// operator does to a healthy one — rotating a secret, clearing a cache. If
// those launches counted, three clicks would turn a fine space unstable and
// the signal would be worth nothing.
func TestRestartServiceDoesNotCountAsInstability(t *testing.T) {
	l := newCounting()
	desired := []Service{{Name: "web", Command: []string{"sh", "-c", "sleep 30"}}}
	a := NewAgent(l, nil, desired, testOpts())
	cancel, wg := startAgent(t, a)
	defer wg.Wait()
	defer cancel()

	waitFor(t, 5*time.Second, "web ready", func() bool {
		return snapshotOf(t, a, "web").State == Ready
	})

	for i := 0; i < 3; i++ {
		if !a.RestartService("web") {
			t.Fatal("RestartService reported an unknown service")
		}
		waitFor(t, 5*time.Second, "web ready again", func() bool {
			return snapshotOf(t, a, "web").State == Ready
		})
	}

	inst, ok := firstInstance(t, a, "web")
	if !ok {
		t.Fatal("web has no instance")
	}
	if inst.Restarts != 0 {
		t.Fatalf("Restarts = %d after 3 operator restarts, want 0", inst.Restarts)
	}
}

// TestCompletedJobReportsNoCrash: every `done` job on a freshly restarted
// production node reported one restart, because the counter was charged on
// entry to onDeath — before the branch that recognises a job exiting 0 as the
// job succeeding. A migrate that ran once and finished is not a symptom.
func TestCompletedJobReportsNoCrash(t *testing.T) {
	l := newCounting()
	desired := []Service{{
		Name:    "migrate",
		Command: []string{"sh", "-c", "exit 0"},
		Oneshot: true,
	}}
	a := NewAgent(l, nil, desired, testOpts())
	cancel, wg := startAgent(t, a)
	defer wg.Wait()
	defer cancel()

	waitFor(t, 5*time.Second, "migrate to complete", func() bool {
		return snapshotOf(t, a, "migrate").State == Done
	})

	inst, ok := firstInstance(t, a, "migrate")
	if !ok {
		t.Fatal("migrate has no instance")
	}
	if inst.Restarts != 0 {
		t.Fatalf("Restarts = %d for a job that exited 0, want 0", inst.Restarts)
	}
}

// A job that exits non-zero IS a symptom, so it still counts.
func TestFailingJobCountsItsCrashes(t *testing.T) {
	l := newCounting()
	desired := []Service{{
		Name:    "migrate",
		Command: []string{"sh", "-c", "exit 1"},
		Oneshot: true,
	}}
	a := NewAgent(l, nil, desired, testOpts())
	cancel, wg := startAgent(t, a)
	defer wg.Wait()
	defer cancel()

	waitFor(t, 10*time.Second, "migrate to be held", func() bool {
		inst, ok := firstInstance(t, a, "migrate")
		return ok && inst.Held
	})

	inst, _ := firstInstance(t, a, "migrate")
	if inst.Restarts == 0 {
		t.Fatal("a job that failed its whole retry cap reports no crash")
	}
}

func TestRolloutHistoryMaxFromEnv(t *testing.T) {
	for _, c := range []struct {
		env  string
		want int
	}{
		{"", defaultRolloutHistory},
		{"20", 20},
		{"1", 1},
		{"0", defaultRolloutHistory},
		{"-3", defaultRolloutHistory},
		{"lots", defaultRolloutHistory},
	} {
		t.Setenv("SKLP_ROLLOUT_HISTORY", c.env)
		if got := rolloutHistoryMax(); got != c.want {
			t.Errorf("SKLP_ROLLOUT_HISTORY=%q: got %d, want %d", c.env, got, c.want)
		}
	}
}
