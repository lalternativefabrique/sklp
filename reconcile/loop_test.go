package reconcile

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type stubSource struct {
	mu   sync.Mutex
	refs []string
	err  error
	hits int
}

func (s *stubSource) Fetch(context.Context, string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hits++
	if s.err != nil {
		return "", s.err
	}
	if len(s.refs) == 0 {
		return "", errors.New("stub exhausted")
	}
	ref := s.refs[0]
	if len(s.refs) > 1 {
		s.refs = s.refs[1:]
	}
	return ref, nil
}

type recorder struct {
	mu   sync.Mutex
	refs []string
	err  error
}

func (r *recorder) Reconcile(_ context.Context, _, ref string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refs = append(r.refs, ref)
	return r.err
}

func (r *recorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.refs...)
}

func quiet() Options {
	return Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func target(src Source) Target {
	return Target{Repo: "acme/widgets", Branch: "main", Source: src}
}

// A pass that finds the commit it already reconciled must do nothing: the loop
// runs every interval, and re-applying an unchanged manifest on every tick
// would restart services for no reason.
func TestPassSkipsAnAlreadyReconciledRef(t *testing.T) {
	src := &stubSource{refs: []string{"sha-1"}}
	rec := &recorder{}
	loop := NewLoop([]Target{target(src)}, []Reconciler{rec}, quiet())

	loop.Pass(context.Background())
	loop.Pass(context.Background())

	if got := rec.seen(); len(got) != 1 {
		t.Fatalf("reconciled %v, want the ref handled once", got)
	}
	if src.hits != 2 {
		t.Errorf("fetched %d times, want both passes to poll", src.hits)
	}
}

func TestPassReconcilesANewCommit(t *testing.T) {
	src := &stubSource{refs: []string{"sha-1", "sha-2"}}
	rec := &recorder{}
	loop := NewLoop([]Target{target(src)}, []Reconciler{rec}, quiet())

	loop.Pass(context.Background())
	loop.Pass(context.Background())

	got := rec.seen()
	if len(got) != 2 || got[0] != "sha-1" || got[1] != "sha-2" {
		t.Fatalf("reconciled %v, want both commits in order", got)
	}
}

// A failed reconcile must be retried on the next pass. Remembering the ref
// anyway would skip it forever, leaving the node one commit behind with
// nothing to say why.
func TestPassRetriesAfterAFailure(t *testing.T) {
	src := &stubSource{refs: []string{"sha-1"}}
	rec := &recorder{err: errors.New("apply refused")}
	loop := NewLoop([]Target{target(src)}, []Reconciler{rec}, quiet())

	loop.Pass(context.Background())
	loop.Pass(context.Background())

	if got := rec.seen(); len(got) != 2 {
		t.Fatalf("reconciled %v, want the failed ref retried", got)
	}
}

// Once it succeeds, the retry stops.
func TestPassStopsRetryingOnceItSucceeds(t *testing.T) {
	src := &stubSource{refs: []string{"sha-1"}}
	rec := &recorder{err: errors.New("transient")}
	loop := NewLoop([]Target{target(src)}, []Reconciler{rec}, quiet())

	loop.Pass(context.Background())
	rec.mu.Lock()
	rec.err = nil
	rec.mu.Unlock()
	loop.Pass(context.Background())
	loop.Pass(context.Background())

	if got := rec.seen(); len(got) != 2 {
		t.Fatalf("reconciled %v, want one failure, one success, then nothing", got)
	}
}

// One repo's credentials expiring must not stop this node's other spaces from
// reconciling.
func TestPassContinuesPastAFailingTarget(t *testing.T) {
	broken := Target{Repo: "acme/broken", Branch: "main", Source: &stubSource{err: errors.New("auth failed")}}
	working := Target{Repo: "acme/working", Branch: "main", Source: &stubSource{refs: []string{"sha-1"}}}
	rec := &recorder{}
	loop := NewLoop([]Target{broken, working}, []Reconciler{rec}, quiet())

	loop.Pass(context.Background())

	if got := rec.seen(); len(got) != 1 || got[0] != "sha-1" {
		t.Fatalf("reconciled %v, want the working repo handled despite the broken one", got)
	}
}

// Every reconciler runs even when an earlier one fails: a broken
// image-watches.yaml must not stop deploy.yaml from reconciling.
func TestPassRunsEveryReconcilerDespiteAFailure(t *testing.T) {
	failing := &recorder{err: errors.New("bad watches file")}
	other := &recorder{}
	loop := NewLoop([]Target{target(&stubSource{refs: []string{"sha-1"}})},
		[]Reconciler{failing, other}, quiet())

	loop.Pass(context.Background())

	if got := other.seen(); len(got) != 1 {
		t.Fatalf("the second reconciler saw %v, want it to run anyway", got)
	}
}

// Run reconciles immediately rather than after one interval: a node that just
// started must converge now, not a minute from now.
func TestRunReconcilesBeforeTheFirstTick(t *testing.T) {
	src := &stubSource{refs: []string{"sha-1"}}
	rec := &recorder{}
	loop := NewLoop([]Target{target(src)}, []Reconciler{rec},
		Options{Interval: time.Hour, Logger: quiet().Logger})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- loop.Run(ctx) }()

	deadline := time.After(2 * time.Second)
	for len(rec.seen()) == 0 {
		select {
		case <-deadline:
			cancel()
			t.Fatal("Run did not reconcile before the first tick")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	cancel()

	if err := <-done; err != nil {
		t.Fatalf("Run returned %v, want nil on cancellation", err)
	}
}

// Cancelling mid-pass stops it rather than working through every remaining
// target: a node shutting down should not start new applies.
func TestPassStopsWhenTheContextIsCancelled(t *testing.T) {
	rec := &recorder{}
	first := Target{Repo: "a", Branch: "main", Source: &stubSource{refs: []string{"sha-1"}}}
	second := Target{Repo: "b", Branch: "main", Source: &stubSource{refs: []string{"sha-2"}}}
	loop := NewLoop([]Target{first, second}, []Reconciler{rec}, quiet())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	loop.Pass(ctx)

	if got := rec.seen(); len(got) != 0 {
		t.Fatalf("reconciled %v on a cancelled context, want nothing", got)
	}
}
