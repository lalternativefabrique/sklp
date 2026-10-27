package spacemanager

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeRunner records backup invocations and returns a canned exit code.
type fakeRunner struct {
	mu        sync.Mutex
	calls     []string // service names, in order
	triggered []string // job names handed to TriggerJob, in order
	exitCode  int
	ok        bool // ResolveExecTarget success
	jobOK     bool // TriggerJob success
	desired   []Service
}

func (f *fakeRunner) DesiredServices() []Service {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Service(nil), f.desired...)
}

func (f *fakeRunner) setDesired(services ...Service) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.desired = services
}

func schedulerOver(fr *fakeRunner, services ...Service) *backupScheduler {
	fr.setDesired(services...)
	return newBackupScheduler(fr, fr.runBackup)
}

func (f *fakeRunner) ResolveExecTarget(name string) (ExecTarget, bool) {
	return ExecTarget{ContainerID: name}, f.ok
}

func (f *fakeRunner) TriggerJob(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.triggered = append(f.triggered, name)
	return f.jobOK
}

func (f *fakeRunner) triggeredSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.triggered...)
}

func scheduledJob(name, schedule string) Service {
	return Service{Name: name, Oneshot: true, Schedule: schedule}
}

func (f *fakeRunner) runBackup(ctx context.Context, target ExecTarget, cmd []string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, target.ContainerID)
	return f.exitCode, nil
}

func (f *fakeRunner) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func backupSvc(name, schedule string) Service {
	return Service{Name: name, Backup: &ServiceBackup{Command: []string{"true"}, Schedule: schedule}}
}

func TestBackupScheduler_FiresWhenDue(t *testing.T) {
	fr := &fakeRunner{ok: true}
	sched := schedulerOver(fr,
		backupSvc("db", "0 3 * * *"),
		backupSvc("cache", "*/5 * * * *"),
		Service{Name: "web"},
	)

	// 03:00 — db is due, cache (multiple of 5) also due.
	sched.tick(context.Background(), time.Date(2026, 6, 7, 3, 0, 0, 0, time.UTC))
	got := fr.snapshot()
	assertContains(t, got, "db")
	assertContains(t, got, "cache")

	// 03:01 — nothing due.
	before := len(fr.snapshot())
	sched.tick(context.Background(), time.Date(2026, 6, 7, 3, 1, 0, 0, time.UTC))
	if len(fr.snapshot()) != before {
		t.Errorf("no backup should fire at 03:01")
	}
}

func TestBackupScheduler_NoDoubleFirePerMinute(t *testing.T) {
	fr := &fakeRunner{ok: true}
	sched := schedulerOver(fr, backupSvc("db", "0 3 * * *"))

	at := time.Date(2026, 6, 7, 3, 0, 0, 0, time.UTC)
	sched.tick(context.Background(), at)
	sched.tick(context.Background(), at.Add(30*time.Second)) // same minute
	if n := len(fr.snapshot()); n != 1 {
		t.Errorf("want 1 fire in the same minute, got %d", n)
	}
	// Next day same time — should fire again.
	sched.tick(context.Background(), at.Add(24*time.Hour))
	if n := len(fr.snapshot()); n != 2 {
		t.Errorf("want 2 fires across two days, got %d", n)
	}
}

func TestBackupScheduler_SkipsWhenNoLiveInstance(t *testing.T) {
	fr := &fakeRunner{ok: false} // ResolveExecTarget fails
	sched := schedulerOver(fr, backupSvc("db", "0 3 * * *"))
	sched.tick(context.Background(), time.Date(2026, 6, 7, 3, 0, 0, 0, time.UTC))
	if n := len(fr.snapshot()); n != 0 {
		t.Errorf("no exec when no live instance, got %d calls", n)
	}
}

func assertContains(t *testing.T, got []string, want string) {
	t.Helper()
	for _, g := range got {
		if g == want {
			return
		}
	}
	t.Errorf("want %q in %v", want, got)
}

func TestJobScheduler_TriggersJobOnSchedule(t *testing.T) {
	fr := &fakeRunner{ok: true, jobOK: true}
	sched := schedulerOver(fr,
		scheduledJob("ship", "*/15 * * * *"),
		Service{Name: "migrate", Oneshot: true},
	)

	sched.tick(context.Background(), time.Date(2026, 6, 7, 3, 15, 0, 0, time.UTC))
	assertContains(t, fr.triggeredSnapshot(), "ship")
	for _, n := range fr.triggeredSnapshot() {
		if n == "migrate" {
			t.Fatal("a job without a schedule must never be triggered")
		}
	}
	// A scheduled job is relaunched, never exec'd into.
	if got := fr.snapshot(); len(got) != 0 {
		t.Fatalf("scheduled job must not exec into a live instance, got %v", got)
	}

	// Same minute: deduped.
	before := len(fr.triggeredSnapshot())
	sched.tick(context.Background(), time.Date(2026, 6, 7, 3, 15, 30, 0, time.UTC))
	if len(fr.triggeredSnapshot()) != before {
		t.Fatal("job fired twice within the same wall-clock minute")
	}

	// Next slot: fires again.
	sched.tick(context.Background(), time.Date(2026, 6, 7, 3, 30, 0, 0, time.UTC))
	if len(fr.triggeredSnapshot()) != before+1 {
		t.Fatalf("job did not fire on its next slot: %v", fr.triggeredSnapshot())
	}
}

// A service `backup:` and a job `schedule:` due on the same minute must both
// fire: they occupy separate dedupe slots.
func TestJobScheduler_BackupAndJobShareNoDedupeSlot(t *testing.T) {
	fr := &fakeRunner{ok: true, jobOK: true}
	sched := schedulerOver(fr,
		backupSvc("db", "0 3 * * *"),
		scheduledJob("ship", "0 3 * * *"),
	)
	sched.tick(context.Background(), time.Date(2026, 6, 7, 3, 0, 0, 0, time.UTC))
	assertContains(t, fr.snapshot(), "db")
	assertContains(t, fr.triggeredSnapshot(), "ship")
}

// A run still in flight must not stack a second one; the scheduler reports the
// skip and moves on rather than treating it as fatal.
func TestJobScheduler_SkipsWhenPreviousRunInFlight(t *testing.T) {
	fr := &fakeRunner{ok: true, jobOK: false}
	sched := schedulerOver(fr, scheduledJob("ship", "* * * * *"))
	sched.tick(context.Background(), time.Date(2026, 6, 7, 3, 0, 0, 0, time.UTC))
	assertContains(t, fr.triggeredSnapshot(), "ship")
}

// A non-oneshot service carrying a schedule is not a scheduled job: only the
// jobs: block can drive a relaunch, and config validation rejects the rest.
func TestJobScheduler_IgnoresScheduleOnService(t *testing.T) {
	fr := &fakeRunner{ok: true, jobOK: true}
	sched := schedulerOver(fr, Service{Name: "web", Schedule: "* * * * *"})
	sched.tick(context.Background(), time.Date(2026, 6, 7, 3, 0, 0, 0, time.UTC))
	if got := fr.triggeredSnapshot(); len(got) != 0 {
		t.Fatalf("a schedule on a non-oneshot service must not trigger, got %v", got)
	}
}

func TestJobScheduler_PicksUpJobsAppliedAfterStart(t *testing.T) {
	fr := &fakeRunner{ok: true, jobOK: true}
	sched := schedulerOver(fr)
	sched.tick(context.Background(), time.Date(2026, 6, 7, 4, 29, 0, 0, time.UTC))

	fr.setDesired(scheduledJob("restore-check", "30 4 * * *"))
	sched.tick(context.Background(), time.Date(2026, 6, 7, 4, 30, 0, 0, time.UTC))
	assertContains(t, fr.triggeredSnapshot(), "restore-check")
}

func TestJobScheduler_FollowsARescheduledJob(t *testing.T) {
	fr := &fakeRunner{ok: true, jobOK: true}
	sched := schedulerOver(fr, scheduledJob("ship", "0 3 * * *"))

	fr.setDesired(scheduledJob("ship", "0 5 * * *"))
	sched.tick(context.Background(), time.Date(2026, 6, 7, 3, 0, 0, 0, time.UTC))
	if got := fr.triggeredSnapshot(); len(got) != 0 {
		t.Fatalf("the old slot fired after the reschedule: %v", got)
	}
	sched.tick(context.Background(), time.Date(2026, 6, 7, 5, 0, 0, 0, time.UTC))
	assertContains(t, fr.triggeredSnapshot(), "ship")
}

func TestBackupScheduler_InvalidScheduleSkipsOnlyThatEntry(t *testing.T) {
	fr := &fakeRunner{ok: true, jobOK: true}
	sched := schedulerOver(fr, scheduledJob("bad", "nope"), scheduledJob("ship", "0 3 * * *"))
	sched.tick(context.Background(), time.Date(2026, 6, 7, 3, 0, 0, 0, time.UTC))
	assertContains(t, fr.triggeredSnapshot(), "ship")
}
