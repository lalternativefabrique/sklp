package rolling

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/reconcile/imageupdate/domain"
)

type fakeDispatcher struct {
	calls    atomic.Int32
	err      error
	tags     []string
	mu       sync.Mutex
	result   string
	rollouts []domain.NodeRollout
}

func (f *fakeDispatcher) Rollouts(context.Context, *domain.Watch) ([]domain.NodeRollout, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.NodeRollout(nil), f.rollouts...), nil
}

func (f *fakeDispatcher) Roll(_ context.Context, _ *domain.Watch, tag string) (string, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.tags = append(f.tags, tag)
	f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	if f.result != "" {
		return f.result, nil
	}
	return domain.RollResultRolled, nil
}

func waitUntil(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", msg)
}

func settleWith(t *testing.T, nodeResult, reason string) (*domain.Watch, *fakeRepo) {
	t.Helper()
	w := testWatch(t, "20260818-0331-d205d58")
	repo := newFakeRepo(w)
	disp := &fakeDispatcher{result: domain.RollResultApplied}
	disp.rollouts = []domain.NodeRollout{{
		Service: w.Service, FromImage: w.ImageRef(w.CurrentTag), ToImage: w.ImageRef("20260820-1349-bff7534"),
		Result: nodeResult, Reason: reason, ExitCode: 3, LogTail: []string{"boom"},
		StartedAt: time.Now(), FinishedAt: time.Now(),
	}}
	h := NewRoller(repo, disp)
	event := domain.RollRequestedEvent{WatchID: w.ID, StackID: w.StackID, FromTag: w.CurrentTag, ToTag: "20260820-1349-bff7534"}
	if err := h.Handle(context.Background(), event); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	repo.mu.Lock()
	tag, status := w.CurrentTag, w.Status
	repo.mu.Unlock()
	if tag != "20260818-0331-d205d58" || status != domain.StatusRolling {
		t.Fatalf("after apply: tag=%q status=%q, want the previous tag and a rolling watch until the node reports", tag, status)
	}
	return w, repo
}

// TestAppliedRollSettlesLive: the tag advances only once the node reports the
// new generation survived its soak.
func TestAppliedRollSettlesLive(t *testing.T) {
	w, repo := settleWith(t, domain.NodeRolloutSucceeded, "")
	waitUntil(t, "roll settled live", func() bool {
		repo.mu.Lock()
		defer repo.mu.Unlock()
		return w.Status == domain.StatusIdle
	})
	if w.CurrentTag != "20260820-1349-bff7534" {
		t.Fatalf("CurrentTag = %q, want the live tag", w.CurrentTag)
	}
	rolls, _ := repo.ListRolls(context.Background(), w.ID, 10)
	if len(rolls) != 1 || rolls[0].Result != domain.RollResultLive || rolls[0].FinishedAt == nil {
		t.Fatalf("roll history = %+v, want one live entry with finished_at", rolls[0])
	}
}

// TestRolledBackRollHoldsTag: a rollback keeps the previous tag, records why,
// and holds the broken tag so the poller does not roll it again.
func TestRolledBackRollHoldsTag(t *testing.T) {
	w, repo := settleWith(t, domain.NodeRolloutRolledBack, "web-v4-0 exited (exit 3) inside the soak window")
	waitUntil(t, "roll settled rolled_back", func() bool {
		repo.mu.Lock()
		defer repo.mu.Unlock()
		return w.Status == domain.StatusHeld
	})
	if w.CurrentTag != "20260818-0331-d205d58" || w.HeldTag != "20260820-1349-bff7534" {
		t.Fatalf("tag=%q held=%q, want the previous tag kept and the broken one held", w.CurrentTag, w.HeldTag)
	}
	rolls, _ := repo.ListRolls(context.Background(), w.ID, 10)
	if r := rolls[0]; r.Result != domain.RollResultRolledBack || r.ExitCode != 3 || r.LogTail != "boom" || r.ErrorMessage == "" {
		t.Fatalf("roll = %+v, want rolled_back with the node's evidence", r)
	}
	if _, ok := w.NewerTag([]string{"20260818-0331-d205d58", "20260820-1349-bff7534"}); ok {
		t.Fatal("the held tag was offered for a new roll")
	}
	if tag, ok := w.NewerTag([]string{"20260820-1349-bff7534", "20260821-0900-abc1234"}); !ok || tag != "20260821-0900-abc1234" {
		t.Fatalf("a newer tag must still roll past the hold, got %q %v", tag, ok)
	}
}

// TestSupersededRollFails: a rollout the node abandoned for a newer spec is
// not live and must not advance the tag.
func TestSupersededRollFails(t *testing.T) {
	w, repo := settleWith(t, domain.NodeRolloutSuperseded, "superseded by a newer spec")
	waitUntil(t, "roll settled failed", func() bool {
		repo.mu.Lock()
		defer repo.mu.Unlock()
		return w.Status == domain.StatusFailed
	})
	if w.CurrentTag != "20260818-0331-d205d58" {
		t.Fatalf("CurrentTag = %q, want it unchanged", w.CurrentTag)
	}
}

// TestRollAdvancesCurrentTag: a successful roll stamps the new tag, so the next
// poll sees nothing to do.
func TestRollAdvancesCurrentTag(t *testing.T) {
	w := testWatch(t, "20260818-0331-d205d58")
	repo := newFakeRepo(w)
	disp := &fakeDispatcher{}
	h := NewRoller(repo, disp)

	event := domain.RollRequestedEvent{
		WatchID: w.ID, StackID: w.StackID,
		FromTag: w.CurrentTag, ToTag: "20260820-1349-bff7534",
	}
	if err := h.Handle(context.Background(), event); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if w.CurrentTag != "20260820-1349-bff7534" {
		t.Fatalf("CurrentTag = %q, want the rolled tag", w.CurrentTag)
	}
	if w.Status != domain.StatusIdle {
		t.Fatalf("status = %q, want idle after a successful roll", w.Status)
	}
	if _, ok := w.NewerTag([]string{"20260818-0331-d205d58", "20260820-1349-bff7534"}); ok {
		t.Error("a further roll was requested for a tag already deployed")
	}
}

// TestConcurrentRollsDispatchOnce is the dedup guarantee. A queue group hands
// each MESSAGE to one subscriber, but two replicas noticing the same new tag
// publish two distinct messages — so the row-level claim is what keeps the node
// from being called twice for one tag.
func TestConcurrentRollsDispatchOnce(t *testing.T) {
	w := testWatch(t, "20260818-0331-d205d58")
	repo := newFakeRepo(w)
	disp := &fakeDispatcher{}
	h := NewRoller(repo, disp)

	event := domain.RollRequestedEvent{
		WatchID: w.ID, StackID: w.StackID,
		FromTag: w.CurrentTag, ToTag: "20260820-1349-bff7534",
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = h.Handle(context.Background(), event)
		}()
	}
	wg.Wait()

	if got := disp.calls.Load(); got != 1 {
		t.Fatalf("node called %d times for one new tag, want exactly 1", got)
	}
}

// TestFailedRollDoesNotAdvanceTag: a failure must be retried on the next tick,
// which it would not be if the tag were recorded as deployed.
func TestFailedRollDoesNotAdvanceTag(t *testing.T) {
	w := testWatch(t, "20260818-0331-d205d58")
	repo := newFakeRepo(w)
	disp := &fakeDispatcher{err: errors.New("node unreachable")}
	h := NewRoller(repo, disp)

	event := domain.RollRequestedEvent{
		WatchID: w.ID, StackID: w.StackID,
		FromTag: w.CurrentTag, ToTag: "20260820-1349-bff7534",
	}
	if err := h.Handle(context.Background(), event); err == nil {
		t.Fatal("expected the node error to surface")
	}
	if w.CurrentTag != "20260818-0331-d205d58" {
		t.Fatalf("CurrentTag = %q, want it unchanged after a failed roll", w.CurrentTag)
	}
	if w.Status != domain.StatusFailed {
		t.Fatalf("status = %q, want failed", w.Status)
	}
	rolls, _ := repo.ListRolls(context.Background(), w.ID, 10)
	if len(rolls) != 1 || rolls[0].Result != domain.RollResultFailed {
		t.Fatalf("roll history = %+v, want one failed entry", rolls)
	}
}

// TestFailedRollRetriesNextTick: after a failure the watch is claimable again,
// so the same tag is attempted on the following tick rather than being stuck.
func TestFailedRollRetriesNextTick(t *testing.T) {
	w := testWatch(t, "20260818-0331-d205d58")
	repo := newFakeRepo(w)
	disp := &fakeDispatcher{err: errors.New("node unreachable")}
	h := NewRoller(repo, disp)

	event := domain.RollRequestedEvent{
		WatchID: w.ID, StackID: w.StackID,
		FromTag: w.CurrentTag, ToTag: "20260820-1349-bff7534",
	}
	_ = h.Handle(context.Background(), event)

	disp.err = nil
	if err := h.Handle(context.Background(), event); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if w.CurrentTag != "20260820-1349-bff7534" {
		t.Fatalf("CurrentTag = %q, want the retry to have rolled", w.CurrentTag)
	}
	if got := disp.calls.Load(); got != 2 {
		t.Fatalf("node called %d times, want 2 (failure then retry)", got)
	}
}

// TestResetOrphansUnblocksWatch: a replica dying mid-roll leaves a row rolling,
// which ClaimRoll refuses forever. Boot recovery is what unblocks it.
func TestResetOrphansUnblocksWatch(t *testing.T) {
	w := testWatch(t, "20260818-0331-d205d58")
	w.Status = domain.StatusRolling
	w.UpdatedAt = time.Now().Add(-2 * time.Hour)
	repo := newFakeRepo(w)

	n, err := repo.ResetOrphans(context.Background(), "core restarted mid-roll")
	if err != nil {
		t.Fatalf("ResetOrphans: %v", err)
	}
	if n != 1 {
		t.Fatalf("recovered %d watches, want 1", n)
	}
	claimed, err := repo.ClaimRoll(context.Background(), w.ID, "20260820-1349-bff7534")
	if err != nil || !claimed {
		t.Fatalf("watch still blocked after recovery (claimed=%v, err=%v)", claimed, err)
	}
}

// TestSettleIgnoresARolloutTheNodeListedBeforeTheRoll: re-rolling a tag the
// node rolled back minutes ago must wait for its own rollout, not settle at
// once on the earlier outcome.
func TestSettleIgnoresARolloutTheNodeListedBeforeTheRoll(t *testing.T) {
	w := testWatch(t, "20260818-0331-d205d58")
	repo := newFakeRepo(w)
	disp := &fakeDispatcher{result: domain.RollResultApplied}
	stale := domain.NodeRollout{
		ID: "web-r11", Service: w.Service, FromImage: w.ImageRef(w.CurrentTag), ToImage: w.ImageRef("20260820-1349-bff7534"),
		Result: domain.NodeRolloutRolledBack, Reason: "web-v11-0 exited (exit 1) 3 times before becoming Ready",
		StartedAt: time.Now().Add(-90 * time.Second), FinishedAt: time.Now().Add(-80 * time.Second),
	}
	disp.rollouts = []domain.NodeRollout{stale}
	h := NewRoller(repo, disp)
	event := domain.RollRequestedEvent{WatchID: w.ID, StackID: w.StackID, FromTag: w.CurrentTag, ToTag: "20260820-1349-bff7534"}
	if err := h.Handle(context.Background(), event); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	repo.mu.Lock()
	status := w.Status
	repo.mu.Unlock()
	if status != domain.StatusRolling {
		t.Fatalf("status = %q right after the roll, want still rolling: the stale rollout was read as this roll's outcome", status)
	}

	disp.mu.Lock()
	disp.rollouts = append(disp.rollouts, domain.NodeRollout{
		ID: "web-r12", Service: w.Service, FromImage: w.ImageRef(w.CurrentTag), ToImage: w.ImageRef("20260820-1349-bff7534"),
		Result: domain.NodeRolloutSucceeded, StartedAt: time.Now(), FinishedAt: time.Now(),
	})
	disp.mu.Unlock()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		repo.mu.Lock()
		settled := w.Status == domain.StatusIdle && w.CurrentTag == "20260820-1349-bff7534"
		repo.mu.Unlock()
		if settled {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("roll never settled on its own rollout")
}

// TestSettleTimeoutStillRecordsTheOutcome: a rollout the node keeps re-rolling
// on the same image never reads terminal, so the observation times out. The
// failure must still be written, on its own context, or the watch stays
// rolling and the poller skips every newer tag.
func TestSettleTimeoutStillRecordsTheOutcome(t *testing.T) {
	w := testWatch(t, "20260818-0331-d205d58")
	repo := newFakeRepo(w)
	disp := &fakeDispatcher{result: domain.RollResultApplied}
	disp.rollouts = []domain.NodeRollout{{
		ID: "core-r2", Service: w.Service, FromImage: w.ImageRef(w.CurrentTag), ToImage: w.ImageRef("20260820-1349-bff7534"),
		Result: domain.NodeRolloutSoaking, StartedAt: time.Now(),
	}}
	h := NewRoller(repo, disp).WithSettleTimeout(200 * time.Millisecond)
	event := domain.RollRequestedEvent{WatchID: w.ID, StackID: w.StackID, FromTag: w.CurrentTag, ToTag: "20260820-1349-bff7534"}
	if err := h.Handle(context.Background(), event); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	waitUntil(t, "the timed-out roll to be recorded", func() bool {
		repo.mu.Lock()
		defer repo.mu.Unlock()
		return w.Status == domain.StatusFailed
	})
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if w.CurrentTag != "20260818-0331-d205d58" {
		t.Fatalf("tag advanced to %q on a roll whose outcome was never observed", w.CurrentTag)
	}
	if !strings.Contains(w.LastError, "not observed within") {
		t.Fatalf("last_error = %q, want the timeout reason", w.LastError)
	}
	rolls := repo.rolls[w.ID]
	if len(rolls) != 1 || rolls[0].Pending() || rolls[0].FinishedAt == nil {
		t.Fatalf("roll row = %+v, want it closed with the outcome", rolls[0])
	}
}
