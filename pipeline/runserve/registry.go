package runserve

import (
	"sync"
	"time"
)

// runRecord is one run's state as known to this node, independent of
// whether a caller is still connected to its stream. handleRun keeps it
// updated as the pipeline progresses; handleRunStatus reads it back.
type runRecord struct {
	mu sync.Mutex

	status   string // one of the Status* constants below
	log      []byte // accumulated log frames, newest appended last
	report   string // set once the run's report frame is emitted
	errText  string // set once the run fails
	finished time.Time

	// cancel stops the run's subprocess tree. Held here rather than in
	// handleRun's frame alone so a LATER request — DELETE /v1/run/{id} — can
	// reach a run whose own HTTP handler is still blocked on it.
	cancel    func()
	cancelled bool
}

const (
	StatusRunning   = "running"
	StatusReport    = "report"
	StatusError     = "error"
	StatusCancelled = "cancelled"
)

func (r *runRecord) appendLog(p string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.log = append(r.log, p...)
}

func (r *runRecord) finish(status, report, errText string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = status
	r.report = report
	r.errText = errText
	r.finished = time.Now()
}

// setCancel arms the record with the run's stopper. handleRun calls it once
// the subprocess context exists; a cancel that arrived BEFORE that (the run
// was still cloning) is honoured immediately, so the request is never lost to
// a race with the run's own startup.
func (r *runRecord) setCancel(fn func()) {
	r.mu.Lock()
	already := r.cancelled
	r.cancel = fn
	r.mu.Unlock()
	if already {
		fn()
	}
}

// requestCancel stops the run and reports whether it was still in flight.
// A run that already finished answers false: there is nothing to stop, and
// the caller must not be told it cancelled something that had ended.
func (r *runRecord) requestCancel() bool {
	r.mu.Lock()
	if r.status != StatusRunning {
		r.mu.Unlock()
		return false
	}
	r.cancelled = true
	fn := r.cancel
	r.mu.Unlock()
	if fn != nil {
		fn()
	}
	return true
}

// wasCancelled reports whether this run was stopped on request, so its
// outcome is recorded as cancelled rather than as whatever error the killed
// subprocess happened to produce.
func (r *runRecord) wasCancelled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancelled
}

func (r *runRecord) snapshot() RunStatusResponse {
	r.mu.Lock()
	defer r.mu.Unlock()
	return RunStatusResponse{
		Status: r.status,
		Log:    string(r.log),
		Report: r.report,
		Error:  r.errText,
	}
}

// registry holds one runRecord per run_id the node has accepted, so a
// caller that lost its connection to /v1/run (a redeploy, a network blip)
// can recover the run's state instead of the run being silently orphaned
// on the node's side. Entries are pruned retentionAfter past completion —
// there is no caller left to ask about a run nobody will poll again.
type registry struct {
	mu      sync.Mutex
	records map[string]*runRecord
}

const retentionAfter = 30 * time.Minute

func newRegistry() *registry {
	return &registry{records: make(map[string]*runRecord)}
}

// start registers runID as running and returns its record. A second start
// with the same runID (a retried POST after a dropped response) reuses the
// existing record rather than resetting its accumulated log.
func (reg *registry) start(runID string) *runRecord {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.prune()
	if r, ok := reg.records[runID]; ok {
		return r
	}
	r := &runRecord{status: StatusRunning}
	reg.records[runID] = r
	return r
}

func (reg *registry) get(runID string) (*runRecord, bool) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	r, ok := reg.records[runID]
	return r, ok
}

// prune must be called with reg.mu held.
func (reg *registry) prune() {
	now := time.Now()
	for id, r := range reg.records {
		r.mu.Lock()
		stale := !r.finished.IsZero() && now.Sub(r.finished) > retentionAfter
		r.mu.Unlock()
		if stale {
			delete(reg.records, id)
		}
	}
}
