package spacemanager

import (
	"context"
	"io"
	"time"

	"github.com/lalternativefabrique/sklp/space"
)

// execTargetResolver is the subset of *Agent the backup scheduler needs: the
// live desired set, a live instance to exec into, and a re-run of a job that
// already completed. Kept narrow so the scheduler is unit-testable without a
// real agent/launcher.
type execTargetResolver interface {
	DesiredServices() []Service
	ResolveExecTarget(name string) (ExecTarget, bool)
	TriggerJob(name string) bool
}

// backupRunner runs cmd inside target and returns its exit code. In production
// it wraps space.RuncExecStreams; tests inject a fake.
type backupRunner func(ctx context.Context, target ExecTarget, cmd []string) (int, error)

// backupEntry is one scheduled directive. A service's `backup:` execs command in
// a live instance; a job's `schedule:` re-runs the job's own container instead,
// which is what job=true selects (command is then unused).
type backupEntry struct {
	service string
	command []string
	expr    string
	job     bool
}

// backupScheduler fires per-service backup commands and scheduled jobs on their
// cron schedule. It is minute-driven: tick() is called once per minute (or
// oftener; firing is deduped per wall-clock minute so a sub-minute tick never
// double-fires). The entries are read from the agent's desired set on every
// tick: a space materialised by its first apply starts with no services, and
// a snapshot taken then never scheduled anything until the agent restarted.
type backupScheduler struct {
	resolver  execTargetResolver
	run       backupRunner
	lastFired map[string]string // dedupeKey -> last fired "YYYY-MM-DDTHH:MM"
	badExpr   map[string]string // dedupeKey -> schedule already reported invalid
	log       func(format string, a ...any)
}

// StartBackupScheduler runs the scheduler over the agent's desired set, ticking
// each minute until ctx is cancelled. logf receives one-line status messages.
func StartBackupScheduler(ctx context.Context, resolver execTargetResolver, logf func(format string, a ...any)) {
	sched := newBackupScheduler(resolver, runBackupViaRunc)
	if logf != nil {
		sched.log = logf
	}
	sched.Run(ctx)
}

func newBackupScheduler(resolver execTargetResolver, run backupRunner) *backupScheduler {
	return &backupScheduler{
		resolver:  resolver,
		run:       run,
		lastFired: map[string]string{},
		badExpr:   map[string]string{},
		log:       func(string, ...any) {},
	}
}

func scheduleEntries(services []Service) []backupEntry {
	var entries []backupEntry
	for _, svc := range services {
		if svc.Backup != nil {
			entries = append(entries, backupEntry{
				service: svc.Name,
				command: svc.Backup.Command,
				expr:    svc.Backup.Schedule,
			})
		}
		if svc.Oneshot && svc.Schedule != "" {
			entries = append(entries, backupEntry{
				service: svc.Name,
				expr:    svc.Schedule,
				job:     true,
			})
		}
	}
	return entries
}

// dedupeKey separates the two entry kinds so a job and a same-named service
// backup never share a lastFired slot.
func (e backupEntry) dedupeKey() string {
	if e.job {
		return "job/" + e.service
	}
	return "backup/" + e.service
}

// minuteKey is the dedupe key: a backup fires at most once per wall-clock minute.
func minuteKey(t time.Time) string { return t.Format("2006-01-02T15:04") }

// tick fires every due backup for the wall-clock minute of now. It is safe to
// call more than once within a minute (deduped) and serialises backups (one at a
// time) so a heavy dump never overlaps another.
func (s *backupScheduler) tick(ctx context.Context, now time.Time) {
	key := minuteKey(now)
	for _, e := range scheduleEntries(s.resolver.DesiredServices()) {
		if s.lastFired[e.dedupeKey()] == key {
			continue
		}
		cron, err := ParseCron(e.expr)
		if err != nil {
			if s.badExpr[e.dedupeKey()] != e.expr {
				s.badExpr[e.dedupeKey()] = e.expr
				s.log("sklp deploy: %s schedule %q ignored: %v\n", e.dedupeKey(), e.expr, err)
			}
			continue
		}
		if !cron.Match(now) {
			continue
		}
		s.lastFired[e.dedupeKey()] = key
		s.fire(ctx, e)
	}
}

// fire runs one due entry. A scheduled job is handed back to the agent, which
// relaunches its container; a backup execs its command inside a live instance.
// Every failure is logged, not fatal: the next schedule retries.
func (s *backupScheduler) fire(ctx context.Context, e backupEntry) {
	if e.job {
		if !s.resolver.TriggerJob(e.service) {
			s.log("sklp deploy: scheduled job %q skipped — previous run still in flight\n", e.service)
			return
		}
		s.log("sklp deploy: scheduled job %q triggered\n", e.service)
		return
	}
	target, ok := s.resolver.ResolveExecTarget(e.service)
	if !ok {
		s.log("sklp deploy: backup %q skipped — no live instance\n", e.service)
		return
	}
	code, err := s.run(ctx, target, e.command)
	switch {
	case err != nil:
		s.log("sklp deploy: backup %q failed to run: %v\n", e.service, err)
	case code != 0:
		s.log("sklp deploy: backup %q exited %d\n", e.service, code)
	default:
		s.log("sklp deploy: backup %q ok\n", e.service)
	}
}

// Run drives the scheduler until ctx is cancelled, ticking every minute.
func (s *backupScheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	s.tick(ctx, time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			s.tick(ctx, t)
		}
	}
}

// runBackupViaRunc is the production backupRunner: it execs the command inside
// the instance with runc, discarding stdout/stderr (the command writes its dump
// to a volume itself). This is the same exec path the control socket uses.
func runBackupViaRunc(ctx context.Context, target ExecTarget, cmd []string) (int, error) {
	return space.RuncExecStreams(ctx, target.RuncBin, target.RuncRoot, target.ContainerID, cmd, nil, io.Discard, io.Discard)
}
