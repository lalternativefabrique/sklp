package domain

import (
	"time"

	"github.com/google/uuid"
)

// Roll results. `applied` is what the node's apply answers; whether the new
// generation ends up serving is reported later by the node's rollout record,
// which settles the roll into `live` or `rolled_back`. `rolled` is the name
// rows written before ADR-013 carry for `applied`; they never settle.
const (
	RollResultApplied    = "applied"
	RollResultRolled     = "rolled"
	RollResultUnchanged  = "unchanged"
	RollResultLive       = "live"
	RollResultRolledBack = "rolled_back"
	RollResultFailed     = "failed"
)

// Roll is one attempt to move a watched service from one tag to another. The
// history exists so a user can answer "when did this last deploy, and why did it
// stop deploying" without reading logs.
type Roll struct {
	ID           string
	WatchID      string
	FromTag      string
	ToTag        string
	Result       string
	ErrorMessage string
	ExitCode     int
	LogTail      string
	CreatedAt    time.Time
	FinishedAt   *time.Time
}

// RollOutcome is what the node reported once a rollout ended.
type RollOutcome struct {
	Result     string
	Reason     string
	ExitCode   int
	LogTail    string
	FinishedAt time.Time
}

// NewRoll builds a roll record in its initial state.
func NewRoll(watchID, fromTag, toTag, result, errMsg string) *Roll {
	return &Roll{
		ID:           uuid.New().String(),
		WatchID:      watchID,
		FromTag:      fromTag,
		ToTag:        toTag,
		Result:       result,
		ErrorMessage: errMsg,
		CreatedAt:    time.Now().UTC(),
	}
}

// Succeeded reports whether the roll reached the node without error.
func (r *Roll) Succeeded() bool {
	switch r.Result {
	case RollResultApplied, RollResultRolled, RollResultUnchanged, RollResultLive:
		return true
	}
	return false
}

// Pending reports whether the node accepted the roll and has not yet told
// whether the new generation is serving.
func (r *Roll) Pending() bool {
	return r.Result == RollResultApplied
}
