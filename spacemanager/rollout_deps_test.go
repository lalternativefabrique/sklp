package spacemanager

import (
	"strings"
	"testing"
	"time"
)

func migrateAndAPI(migrateCmd, apiCmd string) []Service {
	return []Service{
		{Name: "migrate", Oneshot: true, Command: []string{"sh", "-c", migrateCmd}},
		{Name: "api", Command: []string{"sh", "-c", apiCmd}, DependsOn: []string{"migrate"}},
	}
}

func TestRollout_CandidateWaitsForTheNewMigrationRun(t *testing.T) {
	l := newCounting()
	a, cancel, wg := startNewAgent(t, l, nil, migrateAndAPI("exit 0", "sleep 30"), testOpts())
	defer wg.Wait()
	defer cancel()
	waitFor(t, 3*time.Second, "v1 ready", a.AllReady)

	next := migrateAndAPI("sleep 1; exit 0", "sleep 31")
	a.Reconfigure(next[1])
	a.Reconfigure(next[0])

	time.Sleep(500 * time.Millisecond)
	if a.State("migrate") == Done {
		t.Fatal("the new migration run finished too early for this test")
	}
	if n := l.startCount("api-v1-0"); n != 0 {
		t.Fatalf("candidate started %d time(s) before the new migration run completed", n)
	}

	waitFor(t, 5*time.Second, "candidate started after the migration", func() bool {
		return l.startCount("api-v1-0") > 0
	})
	waitFor(t, 5*time.Second, "cutover to the candidate", func() bool {
		rs := a.Rollouts("api")
		return len(rs) > 0 && rs[len(rs)-1].Result == RolloutSoaking
	})
}

func TestRollout_FailedMigrationNeverStartsTheCandidate(t *testing.T) {
	l := newCounting()
	a, cancel, wg := startNewAgent(t, l, nil, migrateAndAPI("exit 0", "sleep 30"), testOpts())
	defer wg.Wait()
	defer cancel()
	waitFor(t, 3*time.Second, "v1 ready", a.AllReady)

	next := migrateAndAPI("exit 1", "sleep 31")
	a.Reconfigure(next[0])
	a.Reconfigure(next[1])

	waitFor(t, 15*time.Second, "rollout aborted on the failed job", func() bool {
		rec, ok := lastRollout(a, "api")
		return ok && rec.Result == RolloutRolledBack
	})
	rec, _ := lastRollout(a, "api")
	if !strings.Contains(rec.Reason, `upstream job "migrate" failed`) {
		t.Fatalf("unexpected reason: %q", rec.Reason)
	}
	if n := l.startCount("api-v1-0"); n != 0 {
		t.Fatalf("candidate started %d time(s) although its migration failed", n)
	}
}

func TestRollout_JobSuccessRetriesTheRejectedDependent(t *testing.T) {
	schema := t.TempDir() + "/schema"
	l := newCounting()
	a, cancel, wg := startNewAgent(t, l, nil, migrateAndAPI("exit 0", "sleep 30"), testOpts())
	defer wg.Wait()
	defer cancel()
	waitFor(t, 3*time.Second, "v1 ready", a.AllReady)

	needsSchema := migrateAndAPI("exit 0", "test -f "+schema+" || exit 1; sleep 30")[1]
	a.Reconfigure(needsSchema)
	waitFor(t, 10*time.Second, "candidate rolled back on the old schema", func() bool {
		rec, ok := lastRollout(a, "api")
		return ok && rec.Result == RolloutRolledBack
	})
	if _, rejected := a.RejectedSpec(needsSchema); !rejected {
		t.Fatal("the failed candidate must be rejected")
	}

	a.Reconfigure(migrateAndAPI("touch "+schema, "")[0])

	waitFor(t, 10*time.Second, "rejected candidate retried and cut over", func() bool {
		rs := a.Rollouts("api")
		return len(rs) > 0 && rs[len(rs)-1].Result == RolloutSoaking
	})
}
