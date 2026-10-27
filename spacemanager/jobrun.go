package spacemanager

import (
	"strconv"
	"time"
)

// jobRunHistoryMax caps how many executions a job keeps.
const jobRunHistoryMax = 20

// JobRunRecord is one execution of a oneshot job. The node keeps the recent
// ones per job and serves them on GET /v1/state, because neither of the two
// things an operator would otherwise read answers "did my run happen": the
// terminal state carries no clock, and the container log file is opened
// O_TRUNC on every launch, so it only ever holds the last run's output. Two
// runs printing the identical line are indistinguishable without this.
type JobRunRecord struct {
	ID       string
	Service  string
	Instance string
	Image    string
	Trigger  string
	ExitCode int
	Failed   bool
	// LogTail is THIS run's captured output, read from the run's own log file
	// (opened O_TRUNC per launch) so a job shorter than the tailer's poll
	// interval still records what it printed, and a re-run never inherits the
	// previous run's lines.
	LogTail    []string
	StartedAt  time.Time
	FinishedAt time.Time
}

// recordJobRunLocked appends one finished execution to the job's history.
// Caller holds a.mu.
func (a *Agent) recordJobRunLocked(inst *instance, exitCode int, failed bool) {
	st := a.services[inst.svcName]
	if st == nil {
		return
	}
	rec := JobRunRecord{
		ID:         newJobRunID(inst.svcName, len(st.runs)),
		Service:    inst.svcName,
		Instance:   inst.id,
		Image:      inst.svc.Image,
		Trigger:    inst.runTrigger,
		ExitCode:   exitCode,
		Failed:     failed,
		LogTail:    a.logs.runLines(inst.id),
		StartedAt:  inst.startedAt,
		FinishedAt: time.Now(),
	}
	st.runs = append(st.runs, rec)
	if len(st.runs) > jobRunHistoryMax {
		st.runs = st.runs[len(st.runs)-jobRunHistoryMax:]
	}
}

func newJobRunID(svcName string, n int) string {
	return svcName + "-run" + strconv.Itoa(n) + "-" + strconv.FormatInt(time.Now().UnixMilli(), 36)
}

// JobRuns returns a job's execution history, oldest first.
func (a *Agent) JobRuns(svcName string) []JobRunRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.services[svcName]
	if st == nil {
		return nil
	}
	return append([]JobRunRecord(nil), st.runs...)
}
