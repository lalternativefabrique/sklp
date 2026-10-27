package spacemanager

import (
	"io"
	"sync"
	"time"

	"github.com/lalternativefabrique/sklp/pipeline/reporter"
)

// stateStatus maps a deploy State to the reporter.Status the CLI live block
// renders. It is pure (no I/O, no locks) so the mapping is unit-tested in
// isolation from the terminal renderer.
//
//   - Pending / Starting / Restarting → running (spinner: work in flight)
//   - Ready                           → passed  (frozen ✓)
//   - Failed                          → failed  (frozen ✗)
//
// Restarting maps to running, not failed: a backed-off relaunch is still the
// service trying to come up, and the agent emits Failed→Restarting on every
// crash loop — surfacing each as a hard failure would be misleading noise.
func stateStatus(s State) reporter.Status {
	switch s {
	case Ready, Done:
		// Done = a oneshot job completed (exit 0): a satisfied, frozen step.
		return reporter.StatusPassed
	case Failed, Degraded:
		return reporter.StatusFailed
	default: // Pending, Starting, Restarting, Running
		return reporter.StatusRunning
	}
}

// ReporterObserver adapts the agent's StateEvent stream onto the reusable
// runner/reporter live block (the same multi-line redraw `sklp run` uses).
// Each space service is one reporter "step": it stays in the live block
// (spinner + elapsed) while Pending/Starting/Restarting, with a sub-line
// echoing the current state, and freezes with a ✓/✗ glyph when it reaches
// Ready/Failed terminally.
//
// The agent calls the observer synchronously under its own mutex, so every
// event arrives on a single goroutine — matching the Reporter contract
// (safe from one goroutine; no external serialisation needed).
type ReporterObserver struct {
	rep   reporter.Reporter
	start time.Time

	mu       sync.Mutex
	started  map[string]bool      // service -> already announced (StepStarted)
	startAt  map[string]time.Time // service -> first-seen time (for summary durs)
	final    map[string]State     // service -> last terminal state seen
	finished map[string]time.Time // service -> when it reached Ready/Failed
}

// streamingReporter is the optional reporter capability we need for the live
// per-service block: EnableStreaming promotes a single step into the redraw
// block, and StepLine feeds the per-service state sub-line.
type streamingReporter interface {
	reporter.Reporter
	reporter.StepStreamer
	EnableStreaming()
	// DisableLinePrefix stops the reporter auto-prefixing each tail line with
	// "[step]": the deploy LogSink already renders `[svc] …` compose-style, so
	// the reporter's own prefix would double it to `[svc] [svc] …`.
	DisableLinePrefix()
}

// NewReporterObserver wires an observer onto w using the shared reporter. It
// honours TTY detection itself (reporter.New picks Pretty vs Plain), so a
// piped/CI sink degrades to one line per transition with zero extra code. The
// services are pre-registered so the live block lists every space service
// from the first frame, even before its first transition.
func NewReporterObserver(w io.Writer, meta SpaceMeta, services []Service) *ReporterObserver {
	return newReporterObserverFrom(reporter.New(w, reporter.Options{}), meta, services)
}

// newReporterObserverFrom is NewReporterObserver with the reporter injected, so
// a test can substitute a spy and assert the exact line a LogSink renders (the
// single-prefix guarantee). Production always passes reporter.New.
func newReporterObserverFrom(rep reporter.Reporter, meta SpaceMeta, services []Service) *ReporterObserver {
	o := &ReporterObserver{
		rep:      rep,
		start:    time.Now(),
		started:  make(map[string]bool),
		startAt:  make(map[string]time.Time),
		final:    make(map[string]State),
		finished: make(map[string]time.Time),
	}
	kind := "permanent"
	if meta.Ephemeral {
		kind = "ephemeral"
	}
	rep.Header("deploying space " + meta.Name + " (" + kind + ")")

	// Promote the reporter into the live redraw block (TTY) so each service
	// gets a spinner row and a state sub-line. Plain reporters ignore this.
	if sr, ok := rep.(streamingReporter); ok {
		sr.EnableStreaming()
		// We prefix container log lines ourselves (`[svc] …`); stop the reporter
		// adding a second "[step]" prefix (the `[svc] [svc]` double-prefix bug).
		sr.DisableLinePrefix()
	}
	for _, svc := range services {
		o.ensureStarted(svc.Name)
	}
	return o
}

// Observer returns the StateObserver the agent calls on every transition.
func (o *ReporterObserver) Observer() StateObserver {
	return func(e StateEvent) { o.onEvent(e) }
}

// LogSink returns the sink the agent feeds container stdout/stderr lines into.
// Each line renders as a sub-line under the service's spinner, prefixed by the
// service name (`[db] db up`), mirroring `docker compose up`.
func (o *ReporterObserver) LogSink() LogSink {
	return func(service, line string) {
		o.ensureStarted(service)
		o.line(service, "["+service+"] "+line)
	}
}

// ensureStarted announces a service to the reporter exactly once. Callers hold
// nothing; ensureStarted guards its own map.
func (o *ReporterObserver) ensureStarted(svc string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.started[svc] {
		return
	}
	o.started[svc] = true
	o.startAt[svc] = time.Now()
	o.rep.StepStarted(svc)
}

// onEvent renders one transition.
func (o *ReporterObserver) onEvent(e StateEvent) {
	o.ensureStarted(e.Service)

	switch e.To {
	case Ready:
		o.freeze(e.Service, Ready, e.Detail)
	case Done:
		// A oneshot job completed: freeze its step as passed, like a Ready service.
		o.freeze(e.Service, Done, e.Detail)
	case Failed:
		// Failed is transient in a crash loop (Failed→Restarting→Starting): we
		// echo it as a sub-line but keep the spinner alive, because the agent
		// will retry. Only the final summary reports a service that never came
		// up. A terminal rollout failure surfaces there too.
		o.line(e.Service, "failed: "+detailText(e))
		o.mu.Lock()
		o.final[e.Service] = Failed
		o.mu.Unlock()
	default: // Pending, Starting, Restarting, Degraded
		o.line(e.Service, e.To.String()+detailSuffix(e))
		o.mu.Lock()
		o.final[e.Service] = e.To
		o.mu.Unlock()
	}
}

// freeze finalises a service row with a terminal glyph + elapsed duration.
func (o *ReporterObserver) freeze(svc string, st State, detail string) {
	o.mu.Lock()
	if o.final[svc] == Ready {
		// Already frozen Ready; ignore duplicate (re-ready after a no-op).
		o.mu.Unlock()
		return
	}
	o.final[svc] = st
	o.finished[svc] = time.Now()
	dur := time.Since(o.startAt[svc])
	o.mu.Unlock()
	o.rep.StepFinished(svc, stateStatus(st), dur, detail)
}

// line pushes a state sub-line under a service's spinner (TTY live block) or
// is dropped by the plain reporter, which already prints the transition.
func (o *ReporterObserver) line(svc, text string) {
	if sr, ok := o.rep.(reporter.StepStreamer); ok {
		sr.StepLine(svc, text, false)
		return
	}
	o.rep.StepProgress(svc, text)
}

// Finalize prints the closing summary once the agent stops (Ctrl-C / TTL /
// error). It reports how many services reached Ready and the total elapsed
// time, or names the ones that never came up. For each not-ready service it
// prints the tail of its container log (via logTail, e.g. Agent.RecentLogLines)
// in a FAILURES section, so the user sees WHY without digging through the cache.
// logTail may be nil (no log streaming): the names are still reported.
func (o *ReporterObserver) Finalize(meta SpaceMeta, services []Service, logTail func(svc string) []string) {
	o.mu.Lock()
	ready := 0
	var notReady []string
	for _, svc := range services {
		// A oneshot job is satisfied at Done; a service at Ready. Both count as
		// "up" for the final banner — a Done job is a met precondition, not a
		// service that never came up.
		if o.final[svc.Name] == Ready || o.final[svc.Name] == Done {
			ready++
		} else {
			notReady = append(notReady, svc.Name)
		}
	}
	o.mu.Unlock()

	// FAILURES section (before the banner): the last log lines of each service
	// that never became Ready — the diagnostic the user needs at a glance.
	if logTail != nil && len(notReady) > 0 {
		var items []reporter.Failure
		for _, svc := range notReady {
			lines := logTail(svc)
			msg := "(no container output captured)"
			if len(lines) > 0 {
				msg = joinLines(lines)
			}
			items = append(items, reporter.Failure{
				Scope:         svc,
				Title:         "never became ready",
				Message:       msg,
				PreserveBytes: true,
			})
		}
		o.rep.Failures("FAILURES", items)
	}

	total := time.Since(o.start)
	status := reporter.StatusPassed
	var fields []reporter.FinalField
	title := "space " + meta.Name
	if len(notReady) == 0 {
		fields = append(fields, reporter.FinalField{
			Label: "services",
			Value: pluralReady(ready),
		})
	} else {
		status = reporter.StatusFailed
		fields = append(fields, reporter.FinalField{
			Label: "ready",
			Value: pluralReady(ready),
		})
		fields = append(fields, reporter.FinalField{
			Label: "not ready",
			Value: joinStrings(notReady),
		})
	}
	o.rep.Finalize(title, status, total, fields, nil)
}
