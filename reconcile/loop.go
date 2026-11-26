// Package reconcile drives the node's pull: it fetches the repos this node
// deploys from and hands each new commit to whatever reconciles it.
//
// The loop owns the cadence and the error handling; it knows nothing about
// deploy.yaml or image watches. That separation is what lets the same loop
// serve both contexts, and what makes it testable without either.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Source fetches a repo and reports the commit its branch points at.
type Source interface {
	Fetch(ctx context.Context, branch string) (string, error)
}

// Reconciler acts on a commit. It is called once per pass per repo, with the
// ref the source resolved.
type Reconciler interface {
	Reconcile(ctx context.Context, repo, ref string) error
}

// ReconcilerFunc adapts a function to Reconciler.
type ReconcilerFunc func(ctx context.Context, repo, ref string) error

func (f ReconcilerFunc) Reconcile(ctx context.Context, repo, ref string) error {
	return f(ctx, repo, ref)
}

// Target is one repo this node reconciles from.
type Target struct {
	// Repo names the repo for logs and for the Reconciler; the Source holds
	// the URL and the credentials.
	Repo   string
	Branch string
	Source Source
	// Reconcilers act on this repo alone, on top of the loop's own: they read
	// the repo's files through its mirror, so they cannot serve another repo.
	Reconcilers []Reconciler
}

// TargetSource answers the repos to reconcile, read again on every pass so a
// repo followed or dropped while the node runs is picked up without a restart.
type TargetSource interface {
	Targets(ctx context.Context) []Target
}

// Loop polls its targets and reconciles the ones whose commit moved.
type Loop struct {
	targets     []Target
	source      TargetSource
	reconcilers []Reconciler
	interval    time.Duration
	log         *slog.Logger

	mu   sync.Mutex
	seen map[string]string // repo -> last reconciled ref
}

// Options configure a Loop.
type Options struct {
	// Interval between passes. Defaults to a minute: a push then reconciles
	// within a minute, which is the latency the pull model trades for not
	// needing an inbound path.
	Interval time.Duration
	Logger   *slog.Logger
}

func NewLoop(targets []Target, reconcilers []Reconciler, opts Options) *Loop {
	interval := opts.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Loop{
		targets:     targets,
		reconcilers: reconcilers,
		interval:    interval,
		log:         log,
		seen:        map[string]string{},
	}
}

// NewTargetLoop polls whatever source answers on each pass.
func NewTargetLoop(source TargetSource, opts Options) *Loop {
	l := NewLoop(nil, nil, opts)
	l.source = source
	return l
}

// Run polls until ctx is cancelled. It returns nil on cancellation: stopping
// because the node is shutting down is not a failure to report.
//
// The first pass runs immediately rather than after one interval, so a node
// that just started converges now instead of a minute from now.
func (l *Loop) Run(ctx context.Context) error {
	l.Pass(ctx)

	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			l.Pass(ctx)
		}
	}
}

// Pass runs one poll over every target. It is exported so an operator command
// (or a test) can force a pass without waiting for the tick.
//
// One target's failure does not stop the others: a repo whose credentials
// expired must not keep the rest of this node's spaces from reconciling.
func (l *Loop) Pass(ctx context.Context) {
	targets := l.targets
	if l.source != nil {
		targets = l.source.Targets(ctx)
		l.forgetAllBut(targets)
	}
	for _, target := range targets {
		if ctx.Err() != nil {
			return
		}
		if err := l.reconcileTarget(ctx, target); err != nil {
			l.log.Warn("reconcile: pass failed",
				"repo", target.Repo, "branch", target.Branch, "error", err)
		}
	}
}

func (l *Loop) reconcileTarget(ctx context.Context, target Target) error {
	ref, err := target.Source.Fetch(ctx, target.Branch)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	if l.alreadyReconciled(target.Repo, ref) {
		return nil
	}

	// Every reconciler runs even if an earlier one fails, and the ref is only
	// remembered when they all succeeded — so a transient failure is retried
	// on the next pass instead of being skipped as "already seen".
	var errs []error
	for _, r := range append(append([]Reconciler(nil), l.reconcilers...), target.Reconcilers...) {
		if rerr := r.Reconcile(ctx, target.Repo, ref); rerr != nil {
			errs = append(errs, rerr)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	l.remember(target.Repo, ref)
	l.log.Info("reconcile: pass complete", "repo", target.Repo, "ref", ref)
	return nil
}

func (l *Loop) alreadyReconciled(repo, ref string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seen[repo] == ref
}

func (l *Loop) remember(repo, ref string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen[repo] = ref
}

// forgetAllBut drops the remembered ref of a repo no longer reconciled, so
// following it again applies it again instead of reading as already seen.
func (l *Loop) forgetAllBut(targets []Target) {
	keep := make(map[string]bool, len(targets))
	for _, t := range targets {
		keep[t.Repo] = true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for repo := range l.seen {
		if !keep[repo] {
			delete(l.seen, repo)
		}
	}
}
