package rolling

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/lalternativefabrique/sklp/reconcile/imageupdate/domain"
)

// Dispatcher is the subset of the node client this handler needs. Declared
// here, not imported, so application stays free of infrastructure imports.
type Dispatcher interface {
	Roll(ctx context.Context, w *domain.Watch, tag string) (string, error)
	Rollouts(ctx context.Context, w *domain.Watch) ([]domain.NodeRollout, error)
}

// rollTimeout bounds one node call. Generous: the node pulls an image before it
// answers, and a cold pull of a large image is minutes, not seconds.
const rollTimeout = 5 * time.Minute

// settleTimeout bounds how long an applied roll is watched for its outcome: the
// node's own readiness timeout plus its soak, with room to spare. settleGrace is
// how long a node may report no rollout at all before it is taken for one that
// predates rollout records, whose roll is then assumed live as it always was.
const (
	settleTimeout  = 8 * time.Minute
	settleGrace    = 45 * time.Second
	settleInterval = 5 * time.Second
	resumeWindow   = time.Hour
)

// recordAttempts bounds the retries of writing an outcome. The write runs on
// its own context: the one that bounded the observation is spent by the time
// the outcome is known, and an outcome never written leaves the watch rolling
// until something else resets it.
const (
	recordAttempts = 5
	recordTimeout  = 15 * time.Second
	recordBackoff  = 2 * time.Second
)

// Roller claims a requested roll, calls the node and records the outcome.
type Roller struct {
	repo       domain.Repository
	dispatcher Dispatcher
	completed  func(domain.RollCompletedEvent)
	settle     time.Duration
	backoff    time.Duration
}

func NewRoller(repo domain.Repository, dispatcher Dispatcher) *Roller {
	return &Roller{repo: repo, dispatcher: dispatcher, settle: settleTimeout, backoff: recordBackoff}
}

// OnCompleted is told every roll that ends, with its outcome: the core
// journals the rolled tag into Git from it.
func (h *Roller) OnCompleted(fn func(domain.RollCompletedEvent)) *Roller {
	h.completed = fn
	return h
}

// RequestRoll drives the roll off the caller's goroutine, so a poller on the
// same process is never held by a node call.
func (h *Roller) RequestRoll(_ context.Context, event domain.RollRequestedEvent) error {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), rollTimeout)
		defer cancel()
		if err := h.Handle(ctx, event); err != nil {
			slog.Error("[imageupdate] roll failed",
				"watch_id", event.WatchID, "to_tag", event.ToTag, "error", err)
		}
	}()
	return nil
}

// WithSettleTimeout bounds how long an applied roll is watched before it is
// recorded as failed. Tests use it; production keeps the default.
func (h *Roller) WithSettleTimeout(d time.Duration) *Roller {
	h.settle = d
	return h
}

// Handle claims the roll, calls the node, and records the outcome.
//
// The claim is what keeps two replicas from rolling the same tag twice: the
// queue group hands each MESSAGE to one subscriber, but two replicas noticing
// the same new tag publish two distinct messages. Losing the claim is a normal
// outcome, not an error.
func (h *Roller) Handle(ctx context.Context, event domain.RollRequestedEvent) error {
	claimed, err := h.repo.ClaimRoll(ctx, event.WatchID, event.ToTag)
	if err != nil {
		return err
	}
	if !claimed {
		slog.Debug("[imageupdate] roll already claimed elsewhere",
			"watch_id", event.WatchID, "to_tag", event.ToTag)
		return nil
	}

	watch, err := h.repo.FindByID(ctx, event.WatchID)
	if err != nil {
		// The claim is already taken; release it so the watch is not stuck
		// rolling forever on a row we can no longer read.
		h.finish(ctx, event, domain.NewRoll(event.WatchID, event.FromTag, event.ToTag,
			domain.RollResultFailed, "load watch: "+err.Error()))
		return err
	}

	prior := h.priorRollouts(ctx, watch)
	result, rollErr := h.dispatcher.Roll(ctx, watch, event.ToTag)
	roll := domain.NewRoll(watch.ID, watch.CurrentTag, event.ToTag, result, "")
	if rollErr != nil {
		roll.Result = domain.RollResultFailed
		roll.ErrorMessage = rollErr.Error()
	}
	h.finish(ctx, event, roll)

	if rollErr != nil {
		return rollErr
	}
	slog.Info("[imageupdate] service rolled",
		"watch", watch.Name, "service", watch.Service, "tag", event.ToTag, "result", roll.Result)
	if roll.Pending() {
		go h.settleRoll(watch, roll, prior)
	}
	return nil
}

// priorRollouts lists the rollouts the node already holds before a roll is
// posted, so settle cannot mistake an earlier rollout of the same tag for the
// one it is watching. Best-effort: a node unreachable here answers the roll
// itself a moment later.
func (h *Roller) priorRollouts(ctx context.Context, watch *domain.Watch) map[string]bool {
	rollouts, err := h.dispatcher.Rollouts(ctx, watch)
	if err != nil {
		return nil
	}
	prior := make(map[string]bool, len(rollouts))
	for _, r := range rollouts {
		if r.ID != "" {
			prior[r.ID] = true
		}
	}
	return prior
}

// ResumePending resumes watching the outcome of every applied roll a previous
// process left unsettled, so a restart mid-rollout still records how it ended.
func (h *Roller) ResumePending(ctx context.Context) {
	rolls, err := h.repo.ListPendingRolls(ctx, time.Now().Add(-resumeWindow))
	if err != nil {
		slog.Error("[imageupdate] could not list pending rolls", "error", err)
		return
	}
	for _, roll := range rolls {
		watch, err := h.repo.FindByID(ctx, roll.WatchID)
		if err != nil {
			continue
		}
		go h.settleRoll(watch, roll, nil)
	}
}

// settleRoll polls the node until the rollout the roll started reaches a
// terminal result, then records it: succeeded → live, rolled_back →
// rolled_back with the node's reason and evidence, anything else → failed.
func (h *Roller) settleRoll(watch *domain.Watch, roll *domain.Roll, prior map[string]bool) {
	ctx, cancel := context.WithTimeout(context.Background(), h.settle)
	outcome := h.observe(ctx, watch, roll, prior)
	cancel()
	h.record(watch, roll, outcome)
}

func (h *Roller) observe(ctx context.Context, watch *domain.Watch, roll *domain.Roll, prior map[string]bool) domain.RollOutcome {
	image := watch.ImageRef(roll.ToTag)
	start := time.Now()
	var lastErr error
	for {
		rollouts, err := h.dispatcher.Rollouts(ctx, watch)
		if err != nil {
			lastErr = err
		} else if r := domain.MatchRolloutExcluding(rollouts, watch.Service, image, roll.CreatedAt, prior); r != nil {
			if r.Terminal() {
				return outcomeOf(r)
			}
		} else if time.Since(start) > settleGrace {
			return domain.RollOutcome{Result: domain.RollResultLive,
				Reason: "node reports no rollout for this image; assumed live", FinishedAt: time.Now()}
		}
		select {
		case <-ctx.Done():
			reason := "rollout outcome not observed within " + h.settle.String()
			if lastErr != nil {
				reason += ": " + lastErr.Error()
			}
			return domain.RollOutcome{Result: domain.RollResultFailed, Reason: reason, FinishedAt: time.Now()}
		case <-time.After(settleInterval):
		}
	}
}

func outcomeOf(r *domain.NodeRollout) domain.RollOutcome {
	out := domain.RollOutcome{Reason: r.Reason, ExitCode: r.ExitCode, FinishedAt: r.FinishedAt,
		LogTail: strings.Join(r.LogTail, "\n")}
	if out.FinishedAt.IsZero() {
		out.FinishedAt = time.Now()
	}
	switch r.Result {
	case domain.NodeRolloutSucceeded:
		out.Result = domain.RollResultLive
	case domain.NodeRolloutRolledBack:
		out.Result = domain.RollResultRolledBack
	default:
		out.Result = domain.RollResultFailed
		if out.Reason == "" {
			out.Reason = "rollout ended " + r.Result
		}
	}
	return out
}

func (h *Roller) record(watch *domain.Watch, roll *domain.Roll, outcome domain.RollOutcome) {
	var err error
	for attempt := 1; attempt <= recordAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), recordTimeout)
		err = h.repo.SettleRoll(ctx, watch.ID, roll, outcome)
		cancel()
		if err == nil {
			break
		}
		slog.Warn("[imageupdate] could not record rollout outcome",
			"watch_id", watch.ID, "roll_id", roll.ID, "attempt", attempt, "error", err)
		time.Sleep(h.backoff)
	}
	if err != nil {
		slog.Error("[imageupdate] rollout outcome lost; the watch stays rolling until the orphan sweep",
			"watch_id", watch.ID, "roll_id", roll.ID, "error", err)
		return
	}
	slog.Info("[imageupdate] rollout settled",
		"watch", watch.Name, "service", watch.Service, "tag", roll.ToTag, "result", outcome.Result, "reason", outcome.Reason)
	settled := *roll
	settled.Result = outcome.Result
	settled.ErrorMessage = outcome.Reason
	settled.CreatedAt = outcome.FinishedAt
	h.publishCompleted(domain.RollRequestedEvent{WatchID: watch.ID, StackID: watch.StackID,
		FromTag: roll.FromTag, ToTag: roll.ToTag}, &settled)
}

func (h *Roller) finish(ctx context.Context, event domain.RollRequestedEvent, roll *domain.Roll) {
	if err := h.repo.FinishRoll(ctx, event.WatchID, roll); err != nil {
		slog.Error("[imageupdate] could not record roll outcome",
			"watch_id", event.WatchID, "error", err)
	}
	h.publishCompleted(event, roll)
}

func (h *Roller) publishCompleted(event domain.RollRequestedEvent, roll *domain.Roll) {
	if h.completed == nil {
		return
	}
	h.completed(domain.RollCompletedEvent{
		WatchID:      event.WatchID,
		StackID:      event.StackID,
		FromTag:      roll.FromTag,
		ToTag:        roll.ToTag,
		Result:       roll.Result,
		ErrorMessage: roll.ErrorMessage,
		CompletedAt:  roll.CreatedAt.Format(time.RFC3339),
	})
}
