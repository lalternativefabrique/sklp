package rolling

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/reconcile/imageupdate/domain"
)

// fakeRepo is an in-memory domain.Repository. ClaimRoll is the interesting
// part: it reproduces the row-level arbitration two replicas race on.
type fakeRepo struct {
	mu      sync.Mutex
	watches map[string]*domain.Watch
	rolls   map[string][]*domain.Roll
	checked map[string]string // watch id -> last check error
	listErr error
}

func newFakeRepo(ws ...*domain.Watch) *fakeRepo {
	r := &fakeRepo{
		watches: map[string]*domain.Watch{},
		rolls:   map[string][]*domain.Roll{},
		checked: map[string]string{},
	}
	for _, w := range ws {
		r.watches[w.ID] = w
	}
	return r
}

func (r *fakeRepo) Save(context.Context, *domain.Watch) error   { return nil }
func (r *fakeRepo) Update(context.Context, *domain.Watch) error { return nil }
func (r *fakeRepo) Delete(context.Context, string) error        { return nil }

func (r *fakeRepo) FindByID(_ context.Context, id string) (*domain.Watch, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.watches[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return w, nil
}

func (r *fakeRepo) FindByStackID(context.Context, string) ([]*domain.Watch, error) { return nil, nil }

func (r *fakeRepo) FindDue(context.Context) ([]*domain.Watch, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domain.Watch
	for _, w := range r.watches {
		if w.Enabled {
			out = append(out, w)
		}
	}
	return out, nil
}

func (r *fakeRepo) ClaimRoll(_ context.Context, watchID, toTag string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.watches[watchID]
	if !ok {
		return false, domain.ErrNotFound
	}
	if w.Status == domain.StatusRolling || w.CurrentTag == toTag {
		return false, nil
	}
	w.Status = domain.StatusRolling
	w.UpdatedAt = time.Now()
	return true, nil
}

func (r *fakeRepo) FinishRoll(_ context.Context, watchID string, roll *domain.Roll) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.watches[watchID]
	if !ok {
		return domain.ErrNotFound
	}
	switch {
	case roll.Pending():
	case roll.Succeeded():
		w.Status = domain.StatusIdle
		w.CurrentTag = roll.ToTag
	default:
		w.Status = domain.StatusFailed
		w.LastError = roll.ErrorMessage
	}
	r.rolls[watchID] = append(r.rolls[watchID], roll)
	return nil
}

func (r *fakeRepo) SettleRoll(ctx context.Context, watchID string, roll *domain.Roll, outcome domain.RollOutcome) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.watches[watchID]
	if !ok {
		return domain.ErrNotFound
	}
	for _, stored := range r.rolls[watchID] {
		if stored.ID == roll.ID {
			stored.Result = outcome.Result
			stored.ErrorMessage = outcome.Reason
			stored.ExitCode = outcome.ExitCode
			stored.LogTail = outcome.LogTail
			at := outcome.FinishedAt
			stored.FinishedAt = &at
		}
	}
	switch outcome.Result {
	case domain.RollResultLive:
		w.Status = domain.StatusIdle
		w.CurrentTag = roll.ToTag
		w.HeldTag = ""
		w.LastError = ""
	case domain.RollResultRolledBack:
		w.Status = domain.StatusHeld
		w.HeldTag = roll.ToTag
		w.PinnedTag = ""
		w.LastError = outcome.Reason
	default:
		w.Status = domain.StatusFailed
		w.LastError = outcome.Reason
	}
	return nil
}

func (r *fakeRepo) ListPendingRolls(_ context.Context, since time.Time) ([]*domain.Roll, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domain.Roll
	for _, rolls := range r.rolls {
		for _, roll := range rolls {
			if roll.Pending() && roll.CreatedAt.After(since) {
				out = append(out, roll)
			}
		}
	}
	return out, nil
}

func (r *fakeRepo) SetPinnedTag(_ context.Context, watchID, tag string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if w, ok := r.watches[watchID]; ok {
		w.PinnedTag = tag
	}
	return nil
}

func (r *fakeRepo) AdoptCurrentTag(_ context.Context, watchID, tag string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if w, ok := r.watches[watchID]; ok && w.Status != domain.StatusRolling {
		w.CurrentTag = tag
	}
	return nil
}

func (r *fakeRepo) Release(_ context.Context, watchID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if w, ok := r.watches[watchID]; ok {
		w.HeldTag, w.PinnedTag, w.LastError = "", "", ""
		if w.Status == domain.StatusHeld || (w.Status == domain.StatusRolling && !r.claimedWithin(w, 10*time.Minute) && !r.pendingRollYoungerThan(watchID, 10*time.Minute)) {
			r.closeLostRolls(watchID, "released by the operator before the outcome was recorded")
			w.Status = domain.StatusIdle
		}
	}
	return nil
}

func (r *fakeRepo) claimedWithin(w *domain.Watch, age time.Duration) bool {
	return w.UpdatedAt.After(time.Now().Add(-age))
}

func (r *fakeRepo) pendingRollYoungerThan(watchID string, age time.Duration) bool {
	for _, roll := range r.rolls[watchID] {
		if roll.Pending() && roll.CreatedAt.After(time.Now().Add(-age)) {
			return true
		}
	}
	return false
}

func (r *fakeRepo) closeLostRolls(watchID, reason string) {
	for _, roll := range r.rolls[watchID] {
		if roll.Pending() {
			roll.Result = domain.RollResultFailed
			roll.ErrorMessage = reason
			at := time.Now()
			roll.FinishedAt = &at
		}
	}
}

func (r *fakeRepo) TouchChecked(_ context.Context, watchID string, _ time.Time, checkErr string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checked[watchID] = checkErr
	return nil
}

func (r *fakeRepo) ListRolls(_ context.Context, watchID string, _ int) ([]*domain.Roll, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rolls[watchID], nil
}

func (r *fakeRepo) ResetOrphans(_ context.Context, reason string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for id, w := range r.watches {
		if w.Status == domain.StatusRolling && !r.claimedWithin(w, 10*time.Minute) && !r.pendingRollYoungerThan(id, time.Hour) {
			r.closeLostRolls(id, reason)
			w.Status = domain.StatusIdle
			w.LastError = reason
			n++
		}
	}
	return n, nil
}

type fakeRegistry struct {
	tags map[string][]string
	err  error
}

func (f *fakeRegistry) ListTags(_ context.Context, repo, _, _ string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.tags[repo], nil
}

func testWatch(t *testing.T, currentTag string) *domain.Watch {
	t.Helper()
	w, err := domain.NewWatch("stack-1", "lalter-www", "rg.example.com/ns/www",
		"", "www", "https://node:6464", "", "tok", "")
	if err != nil {
		t.Fatalf("NewWatch: %v", err)
	}
	w.CurrentTag = currentTag
	return w
}

// TestPollStampsCheckErrorPerWatch: an unreachable registry is recorded on the
// watch's own row and does not fail the pass. One broken watch must not stop
// every other watch from deploying.
func TestPollStampsCheckErrorPerWatch(t *testing.T) {
	w := testWatch(t, "20260818-0331-d205d58")
	repo := newFakeRepo(w)
	reg := &fakeRegistry{err: errors.New("registry unreachable")}

	p := NewPoller(repo, reg, nil, time.Minute)
	if err := p.Poll(context.Background()); err != nil {
		t.Fatalf("Poll returned an error for a single failing watch: %v", err)
	}
	if repo.checked[w.ID] == "" {
		t.Error("the check error was not stamped on the watch")
	}
}

// TestPollFailsWhenListingWatchesFails: the pass itself failing IS an error,
// since it is what the circuit breaker counts.
func TestPollFailsWhenListingWatchesFails(t *testing.T) {
	repo := newFakeRepo()
	repo.listErr = errors.New("db down")
	p := NewPoller(repo, &fakeRegistry{}, nil, time.Minute)
	if err := p.Poll(context.Background()); err == nil {
		t.Fatal("expected an error when the watch listing fails")
	}
}

// TestCircuitBreakerStopsAfterRepeatedFailures: the poller gives up rather than
// logging the same error every tick forever, and the status carries a reason.
func TestCircuitBreakerStopsAfterRepeatedFailures(t *testing.T) {
	p := NewPoller(newFakeRepo(), &fakeRegistry{}, nil, time.Minute)
	err := errors.New("db down")
	for i := 1; i < maxConsecutivePollErrors; i++ {
		if p.recordPollError(err) {
			t.Fatalf("gave up after %d errors, before the limit", i)
		}
	}
	if !p.recordPollError(err) {
		t.Fatal("expected the poller to give up at the limit")
	}
	st := p.Status()
	if st.State != PollerStateFailed {
		t.Errorf("state = %q, want failed", st.State)
	}
	if st.StopReason == "" || st.LastError == "" {
		t.Errorf("status carries no actionable reason: %+v", st)
	}
}

// TestPollSuccessResetsErrorCount: a passing tick clears the streak, so
// intermittent failures never accumulate to a shutdown.
func TestPollSuccessResetsErrorCount(t *testing.T) {
	p := NewPoller(newFakeRepo(), &fakeRegistry{}, nil, time.Minute)
	p.recordPollError(errors.New("transient"))
	p.recordPollError(errors.New("transient"))
	p.recordPollSuccess()
	if got := p.Status().ConsecutiveErrors; got != 0 {
		t.Fatalf("ConsecutiveErrors = %d after a success, want 0", got)
	}
}

type fakeLive struct {
	image string
	err   error
}

func (f *fakeLive) RunningImage(context.Context, *domain.Watch) (string, error) {
	return f.image, f.err
}

// TestPollAdoptsTheTagTheNodeRuns: lungor 2026-10-07. A full apply put the
// node on an older tag than the watch's last roll; the watch must restart
// from what runs, not from what it last rolled.
func TestPollAdoptsTheTagTheNodeRuns(t *testing.T) {
	w := testWatch(t, "20261007-0623-2946eff")
	repo := newFakeRepo(w)
	reg := &fakeRegistry{tags: map[string][]string{w.ImageRepo: {"20260930-0955-92cb56e"}}}
	live := &fakeLive{image: w.ImageRepo + ":20260930-0955-92cb56e"}

	p := NewPoller(repo, reg, nil, time.Minute).WithLiveImages(live)
	if err := p.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if got := repo.watches[w.ID].CurrentTag; got != "20260930-0955-92cb56e" {
		t.Fatalf("current tag = %q, want the tag the node runs", got)
	}
}

func TestPollIgnoresLiveImageWhileRolling(t *testing.T) {
	w := testWatch(t, "20261007-0623-2946eff")
	w.Status = domain.StatusRolling
	repo := newFakeRepo(w)
	repo.rolls[w.ID] = []*domain.Roll{domain.NewRoll(w.ID, w.CurrentTag, "20261007-0700-aaaaaaa", domain.RollResultApplied, "")}
	live := &fakeLive{image: w.ImageRepo + ":20260930-0955-92cb56e"}

	p := NewPoller(repo, &fakeRegistry{}, nil, time.Minute).WithLiveImages(live)
	if err := p.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if got := repo.watches[w.ID].CurrentTag; got != "20261007-0623-2946eff" {
		t.Fatalf("current tag = %q, a roll in flight must not be overwritten", got)
	}
}

func TestPollIgnoresLiveImageOutsideTheRepo(t *testing.T) {
	w := testWatch(t, "20261007-0623-2946eff")
	repo := newFakeRepo(w)
	live := &fakeLive{image: "rg.example.com/other:20260930-0955-92cb56e"}

	p := NewPoller(repo, &fakeRegistry{}, nil, time.Minute).WithLiveImages(live)
	if err := p.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if got := repo.watches[w.ID].CurrentTag; got != "20261007-0623-2946eff" {
		t.Fatalf("current tag = %q, an image from another repository is not this watch's", got)
	}
}

// TestPollSweepsAWatchWhoseOutcomeWasLost: a watch still rolling on a roll
// older than the pending window had its outcome lost; the next pass frees it
// and closes the roll, without a restart.
func TestPollSweepsAWatchWhoseOutcomeWasLost(t *testing.T) {
	w := testWatch(t, "20260818-0331-d205d58")
	w.Status = domain.StatusRolling
	w.UpdatedAt = time.Now().Add(-2 * time.Hour)
	repo := newFakeRepo(w)
	lost := domain.NewRoll(w.ID, w.CurrentTag, "20260820-1349-bff7534", domain.RollResultApplied, "")
	lost.CreatedAt = time.Now().Add(-2 * time.Hour)
	repo.rolls[w.ID] = []*domain.Roll{lost}
	fresh := testWatch(t, "20260818-0331-d205d58")
	fresh.ID = "fresh"
	fresh.Status = domain.StatusRolling
	repo.watches[fresh.ID] = fresh
	repo.rolls[fresh.ID] = []*domain.Roll{domain.NewRoll(fresh.ID, fresh.CurrentTag, "20260820-1349-bff7534", domain.RollResultApplied, "")}

	p := NewPoller(repo, &fakeRegistry{tags: map[string][]string{}}, nil, time.Minute)
	if err := p.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if w.Status != domain.StatusIdle || lost.Pending() {
		t.Fatalf("stuck watch: status=%q roll=%q, want idle with the roll closed", w.Status, lost.Result)
	}
	if fresh.Status != domain.StatusRolling {
		t.Fatalf("a roll still inside the pending window was swept (status=%q)", fresh.Status)
	}
}

// TestReleaseFreesAStuckRollingWatch: the operator's release is the manual
// way out of a rolling watch whose roll is past the settle window; a roll
// still inside it is left alone.
func TestReleaseFreesAStuckRollingWatch(t *testing.T) {
	w := testWatch(t, "20260818-0331-d205d58")
	w.Status = domain.StatusRolling
	repo := newFakeRepo(w)
	roll := domain.NewRoll(w.ID, w.CurrentTag, "20260820-1349-bff7534", domain.RollResultApplied, "")
	repo.rolls[w.ID] = []*domain.Roll{roll}

	if err := repo.Release(context.Background(), w.ID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if w.Status != domain.StatusRolling {
		t.Fatalf("a roll one second old was released (status=%q)", w.Status)
	}
	roll.CreatedAt = time.Now().Add(-11 * time.Minute)
	w.UpdatedAt = roll.CreatedAt
	if err := repo.Release(context.Background(), w.ID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if w.Status != domain.StatusIdle || roll.Pending() {
		t.Fatalf("status=%q roll=%q, want idle with the roll closed", w.Status, roll.Result)
	}
}

// TestPollLeavesAWatchClaimedMomentsAgo: between the claim and the node's
// answer the watch is rolling with no roll row yet, the node still pulling
// the image. Sweeping it there would let the next tick claim the tag again.
func TestPollLeavesAWatchClaimedMomentsAgo(t *testing.T) {
	w := testWatch(t, "20260818-0331-d205d58")
	repo := newFakeRepo(w)
	if claimed, err := repo.ClaimRoll(context.Background(), w.ID, "20260820-1349-bff7534"); err != nil || !claimed {
		t.Fatalf("ClaimRoll: claimed=%v err=%v", claimed, err)
	}
	p := NewPoller(repo, &fakeRegistry{tags: map[string][]string{}}, nil, time.Minute)
	if err := p.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if w.Status != domain.StatusRolling {
		t.Fatalf("status = %q, a watch claimed seconds ago was swept", w.Status)
	}
}

// TestPollSweepsAClaimTheNodeNeverAnswered: skalpai-core 2026-11-04 12:42.
// The process that claimed the roll was stopped by the very rollout it had
// started, before the node answered; no roll row was ever written. Past the
// node call's timeout the claim is lost and the watch must be freed.
func TestPollSweepsAClaimTheNodeNeverAnswered(t *testing.T) {
	w := testWatch(t, "20260818-0331-d205d58")
	w.Status = domain.StatusRolling
	w.UpdatedAt = time.Now().Add(-11 * time.Minute)
	repo := newFakeRepo(w)
	p := NewPoller(repo, &fakeRegistry{tags: map[string][]string{}}, nil, time.Minute)
	if err := p.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if w.Status != domain.StatusIdle {
		t.Fatalf("status = %q, want idle: the claim is older than the node call could last", w.Status)
	}
}
