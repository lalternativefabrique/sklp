package domain

const (
	// SubjectRollRequested fires when the poller finds a tag newer than the one
	// a watch currently runs. Consumed by the dispatcher, which calls the node.
	//
	// The publish/subscribe hop is what makes the poller safe under multiple
	// replicas: every pod tickers, so every pod may notice the same new tag, but
	// a queue group hands each MESSAGE to exactly one subscriber. The dedup
	// window in the dispatcher covers the remaining case — two pods noticing the
	// same tag within one tick and publishing two distinct messages.
	SubjectRollRequested = "imageupdate.roll.requested"

	// QueueDispatcher is the queue group the dispatcher subscribes under. A plain
	// Subscribe would roll the same service once per replica: three pods, three
	// POSTs to the node, three bounces of a healthy service.
	QueueDispatcher = "imageupdate-dispatcher"

	// SubjectRollCompleted fires when a roll reaches a terminal state. Carries
	// the outcome so the UI can render it without a follow-up query.
	SubjectRollCompleted = "imageupdate.roll.completed"
)

// RollRequestedEvent is the payload published on SubjectRollRequested.
type RollRequestedEvent struct {
	WatchID string `json:"watch_id"`
	StackID string `json:"stack_id"`
	FromTag string `json:"from_tag,omitempty"`
	ToTag   string `json:"to_tag"`
}

// RollCompletedEvent is published when a roll finishes, successfully or not.
type RollCompletedEvent struct {
	WatchID      string `json:"watch_id"`
	StackID      string `json:"stack_id"`
	FromTag      string `json:"from_tag,omitempty"`
	ToTag        string `json:"to_tag"`
	Result       string `json:"result"`
	ErrorMessage string `json:"error_message,omitempty"`
	CompletedAt  string `json:"completed_at"` // RFC3339
}
