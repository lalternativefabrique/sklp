package reconcile

import (
	"context"
	"log/slog"
	"sync"

	"github.com/lalternativefabrique/sklp/reconcile/gitsource"
)

// Runner answers the repos this node reconciles: those reconcile.yaml names,
// then those it was told to follow over its control API. Each keeps its own
// mirror and reconcilers for as long as it stays in the set.
type Runner struct {
	stores    *Stores
	node      *LocalNode
	declared  []TargetConfig
	cacheRoot string
	log       *slog.Logger

	mu    sync.Mutex
	built map[string]builtTarget
}

type builtTarget struct {
	repo, branch string
	target       Target
}

func NewRunner(stores *Stores, node *LocalNode, declared []TargetConfig, cacheRoot string, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	return &Runner{stores: stores, node: node, declared: declared, cacheRoot: cacheRoot, log: log, built: map[string]builtTarget{}}
}

// Targets implements TargetSource.
func (r *Runner) Targets(ctx context.Context) []Target {
	wanted := make([]TargetConfig, 0, len(r.declared))
	names := map[string]bool{}
	for _, t := range r.declared {
		if t.Name == "" {
			t.Name = RepoName(t.Repo)
		}
		names[t.Name] = true
		wanted = append(wanted, t)
	}
	follows, err := r.stores.Follows.List(ctx)
	if err != nil {
		r.log.Warn("reconcile: cannot read followed repos", "error", err)
	}
	for _, f := range follows {
		if names[f.Name] {
			r.log.Warn("reconcile: a followed repo shares its name with one reconcile.yaml declares; the declared one wins", "name", f.Name)
			continue
		}
		names[f.Name] = true
		wanted = append(wanted, TargetConfig{Name: f.Name, Repo: f.Repo, Branch: f.Branch})
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Target, 0, len(wanted))
	for _, t := range wanted {
		branch := t.Branch
		if branch == "" {
			branch = "main"
		}
		b, ok := r.built[t.Name]
		if !ok || b.repo != t.Repo || b.branch != branch {
			mirror := gitsource.New(t.Repo, MirrorDir(r.cacheRoot, t.Name))
			b = builtTarget{repo: t.Repo, branch: branch, target: Target{
				Repo:        t.Name,
				Branch:      branch,
				Source:      mirror,
				Reconcilers: BuildReconcilers(r.stores, r.node, mirror, nil),
			}}
			r.built[t.Name] = b
		}
		out = append(out, b.target)
	}
	for name := range r.built {
		if !names[name] {
			delete(r.built, name)
		}
	}
	return out
}
