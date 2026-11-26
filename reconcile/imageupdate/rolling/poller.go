package rolling

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/lalternativefabrique/sklp/reconcile/imageupdate/domain"
)

const maxConsecutivePollErrors = 5

// DefaultPollInterval is how often the registry is checked. A publish is not an
// event anyone waits on at the second, and a shorter interval multiplies
// registry calls across every watch for no gain.
const DefaultPollInterval = 2 * time.Minute

type PollerState string

const (
	PollerStateStopped  PollerState = "stopped"
	PollerStateRunning  PollerState = "running"
	PollerStateStopping PollerState = "stopping"
	PollerStateFailed   PollerState = "failed"
)

type PollerStatus struct {
	Running              bool        `json:"running"`
	State                PollerState `json:"state"`
	ConsecutiveErrors    int         `json:"consecutive_errors"`
	MaxConsecutiveErrors int         `json:"max_consecutive_errors"`
	LastError            string      `json:"last_error,omitempty"`
	StopReason           string      `json:"stop_reason,omitempty"`
	StoppedAt            string      `json:"stopped_at,omitempty"`
}

// TagLister is the subset of the registry client the poller needs.
type TagLister interface {
	ListTags(ctx context.Context, repo, username, secret string) ([]string, error)
}

// Requester hands a roll to whatever drives it: a queue the core's replicas
// share, or a node's own Roller in-process.
type Requester interface {
	RequestRoll(ctx context.Context, event domain.RollRequestedEvent) error
}

// Poller checks every enabled watch's registry and requests a roll when a
// newer tag appears.
//
// It only REQUESTS: the node call happens in the Roller, behind a row-level
// claim. That split is deliberate — every core replica runs this ticker, so the
// check is duplicated by design, and the deduplication has to live where the
// side effect is, not where the observation is.
type Poller struct {
	repo     domain.Repository
	registry TagLister
	// creds resolves the stack's SHARED registry credentials — the default a
	// watch without its own secret authenticates with. nil disables the
	// fallback (older wiring, tests).
	creds     domain.RegistryCredentialRepository
	live      LiveImageReader
	requester Requester
	interval  time.Duration

	mu                sync.Mutex
	state             PollerState
	cancel            context.CancelFunc
	done              chan struct{}
	consecutiveErrors int
	lastError         string
	stopReason        string
	stoppedAt         time.Time
}

// LiveImageReader answers the image a watch's service runs on its node.
type LiveImageReader interface {
	RunningImage(ctx context.Context, w *domain.Watch) (string, error)
}

func NewPoller(repo domain.Repository, registry TagLister, requester Requester, interval time.Duration) *Poller {
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	return &Poller{
		repo:      repo,
		registry:  registry,
		requester: requester,
		interval:  interval,
		state:     PollerStateStopped,
	}
}

// Start launches the poller in a goroutine. No-op if already running.
func (p *Poller) Start() {
	p.mu.Lock()
	if p.state == PollerStateRunning || p.state == PollerStateStopping {
		p.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	p.state = PollerStateRunning
	p.consecutiveErrors = 0
	p.lastError = ""
	p.stopReason = ""
	p.stoppedAt = time.Time{}
	p.mu.Unlock()

	go p.loop(ctx, p.done)
	log.Printf("[imageupdate-poller] started (interval=%s)", p.interval)
}

// Stop stops the poller and waits for the current loop to exit.
func (p *Poller) Stop() {
	p.mu.Lock()
	if p.state == PollerStateStopped || p.state == PollerStateFailed {
		p.mu.Unlock()
		return
	}
	done := p.done
	if p.state == PollerStateRunning {
		p.state = PollerStateStopping
		p.stopReason = "manual_stop"
	}
	p.cancel()
	p.mu.Unlock()

	if done != nil {
		<-done
	}
	p.mu.Lock()
	if p.state == PollerStateStopping {
		p.state = PollerStateStopped
		p.stoppedAt = time.Now().UTC()
	}
	p.mu.Unlock()
	log.Println("[imageupdate-poller] stopped")
}

func (p *Poller) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state == PollerStateRunning
}

func (p *Poller) Status() PollerStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	status := PollerStatus{
		Running:              p.state == PollerStateRunning,
		State:                p.state,
		ConsecutiveErrors:    p.consecutiveErrors,
		MaxConsecutiveErrors: maxConsecutivePollErrors,
		LastError:            p.lastError,
		StopReason:           p.stopReason,
	}
	if !p.stoppedAt.IsZero() {
		status.StoppedAt = p.stoppedAt.Format(time.RFC3339)
	}
	return status
}

func (p *Poller) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			err := p.Poll(ctx)
			if err == nil {
				p.recordPollSuccess()
				continue
			}
			if errors.Is(err, context.Canceled) {
				return
			}
			if p.recordPollError(err) {
				return
			}
		}
	}
}

// Poll checks every enabled watch once. A single watch failing (an unreachable
// registry, a bad credential) is recorded on its own row and does not fail the
// pass: one broken watch must not stop every other one from deploying.
// orphanReason is written on a watch the sweep frees: still rolling, but its
// applied roll is older than any observation of it could have lasted.
const orphanReason = "rollout outcome never recorded; roll closed by the poller"

func (p *Poller) Poll(ctx context.Context) error {
	if n, err := p.repo.ResetOrphans(ctx, orphanReason); err != nil {
		log.Printf("[imageupdate-poller] orphan sweep: %v", err)
	} else if n > 0 {
		log.Printf("[imageupdate-poller] reset %d watch(es) whose rollout outcome was never recorded", n)
	}
	watches, err := p.repo.FindDue(ctx)
	if err != nil {
		return fmt.Errorf("list watches: %w", err)
	}
	for _, w := range watches {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p.checkOne(ctx, w)
	}
	return nil
}

// WithCredentials wires the shared registry-credentials fallback.
func (p *Poller) WithCredentials(creds domain.RegistryCredentialRepository) *Poller {
	p.creds = creds
	return p
}

func (p *Poller) WithLiveImages(live LiveImageReader) *Poller {
	p.live = live
	return p
}

// adoptLiveTag aligns CurrentTag with what the node runs. A full apply that
// changed the service's tag in Git, a rollback or a hand roll all move the
// node without this watch knowing; comparing the registry against a tag
// nobody runs would leave the service wherever it was.
func (p *Poller) adoptLiveTag(ctx context.Context, w *domain.Watch) {
	if p.live == nil || w.Status == domain.StatusRolling {
		return
	}
	image, err := p.live.RunningImage(ctx, w)
	if err != nil {
		log.Printf("[imageupdate-poller] watch %q: live image unknown: %v", w.Name, err)
		return
	}
	tag, ok := strings.CutPrefix(image, w.ImageRepo+":")
	if !ok || tag == w.CurrentTag || !w.TagMatches(tag) {
		return
	}
	if err := p.repo.AdoptCurrentTag(ctx, w.ID, tag); err != nil {
		log.Printf("[imageupdate-poller] watch %q: adopt live tag %s: %v", w.Name, tag, err)
		return
	}
	log.Printf("[imageupdate-poller] watch %q: node runs %s, watch believed %s; adopted", w.Name, tag, w.CurrentTag)
	w.CurrentTag = tag
}

func (p *Poller) checkOne(ctx context.Context, w *domain.Watch) {
	if w.PinnedTag != "" {
		return
	}
	p.adoptLiveTag(ctx, w)
	now := time.Now().UTC()
	user, secret := w.RegistryUser, w.RegistrySecret
	if secret == "" && p.creds != nil {
		if all, cerr := p.creds.ListByStack(ctx, w.StackID); cerr == nil {
			if c := domain.MatchCredential(all, w.ImageRepo); c != nil {
				secret = c.Secret
				if c.User != "" {
					user = c.User
				}
			}
		}
	}
	tags, err := p.registry.ListTags(ctx, w.ImageRepo, user, secret)
	if err != nil {
		_ = p.repo.TouchChecked(ctx, w.ID, now, err.Error())
		log.Printf("[imageupdate-poller] watch %q: list tags: %v", w.Name, err)
		return
	}
	_ = p.repo.TouchChecked(ctx, w.ID, now, "")

	tag, found := w.NewerTag(tags)
	if !found {
		return
	}
	if p.requester == nil {
		return
	}
	if err := p.requester.RequestRoll(ctx, domain.RollRequestedEvent{
		WatchID: w.ID,
		StackID: w.StackID,
		FromTag: w.CurrentTag,
		ToTag:   tag,
	}); err != nil {
		log.Printf("[imageupdate-poller] watch %q: publish roll: %v", w.Name, err)
		return
	}
	log.Printf("[imageupdate-poller] watch %q: %s -> %s requested", w.Name, w.CurrentTag, tag)
}

func (p *Poller) recordPollSuccess() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.consecutiveErrors = 0
	p.lastError = ""
}

// recordPollError counts a failed pass and reports whether the poller should
// give up. Stopping after a run of failures beats logging the same error every
// tick forever: the status endpoint then carries a reason a user can act on.
func (p *Poller) recordPollError(err error) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.consecutiveErrors++
	p.lastError = err.Error()
	log.Printf("[imageupdate-poller] poll failed (%d/%d): %v",
		p.consecutiveErrors, maxConsecutivePollErrors, err)
	if p.consecutiveErrors < maxConsecutivePollErrors {
		return false
	}
	p.state = PollerStateFailed
	p.stopReason = fmt.Sprintf("stopped after %d consecutive errors", p.consecutiveErrors)
	p.stoppedAt = time.Now().UTC()
	log.Printf("[imageupdate-poller] %s", p.stopReason)
	return true
}
