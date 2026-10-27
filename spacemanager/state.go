package spacemanager

// State is the observable lifecycle of one managed service instance. It is the
// substrate a reporter (terminal / Skalpai UI, later milestones) renders as
// "deploying api... -> ready (3.2s)". Milestone 2 only emits the transitions.
type State int

const (
	// Pending: known desired, not yet launched (or awaiting a restart delay).
	Pending State = iota
	// Starting: launched, init PID alive, readiness not yet confirmed.
	Starting
	// Ready: init alive AND readiness probe passed (or no probe = up).
	Ready
	// Failed: readiness timed out/errored, or the init died before ready.
	Failed
	// Restarting: scheduled for a backed-off relaunch after a death/failure.
	Restarting
	// Running: a oneshot job's init is alive and we are awaiting its exit. Unlike
	// a service it has no readiness gate; an exit is EXPECTED, not a crash.
	Running
	// Done: a oneshot job's process exited 0. Terminal — never relaunched, and a
	// satisfied precondition for its dependents. Excluded from AllReady's "every
	// service Ready" accounting (a Done job is met, not a running service).
	Done
	// Degraded: init alive, was Ready, but the readiness probe has failed
	// repeatedly since. Out of the proxy, not restarted: a wrong contract is
	// not fixed by relaunching the same process.
	Degraded
)

// String renders a State for logs and reporters.
func (s State) String() string {
	switch s {
	case Pending:
		return "pending"
	case Starting:
		return "starting"
	case Ready:
		return "ready"
	case Failed:
		return "failed"
	case Restarting:
		return "restarting"
	case Running:
		return "running"
	case Done:
		return "done"
	case Degraded:
		return "degraded"
	default:
		return "unknown"
	}
}

// StateEvent is one observed transition of a service. detail is a short
// human-readable cause (probe duration, exit code, error), empty when none.
type StateEvent struct {
	Service string
	From    State
	To      State
	Detail  string
}

// StateObserver is the injected sink for transitions. The agent calls it on
// every state change (synchronously, off the hot path), so a future reporter
// can drive a UI without the agent knowing about it. nil = no observer.
type StateObserver func(StateEvent)
