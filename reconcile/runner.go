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

	mu     sync.Mutex
	built  map[string]builtTarget
	purged bool
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
	if !r.purged {
		r.purgeNodeScope(ctx)
	}
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
				Reconcilers: BuildRepoReconcilers(r.stores, r.node, mirror, nil, t.Name),
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

// purgeNodeScope drops the records kept under the node-wide scope every repo
// shared before each got its own. Left in place, a watch there would keep
// rolling alongside the one its repo now syncs under its own scope.
func (r *Runner) purgeNodeScope(ctx context.Context) {
	legacy := string(NodeScope(r.node.Name))
	watches, werr := r.stores.Watches.FindByStackID(ctx, legacy)
	deployments, derr := r.stores.Deployments.FindByStackID(ctx, legacy)
	if werr != nil || derr != nil {
		r.log.Warn("reconcile: cannot read the shared-scope records yet", "watches", werr, "deployments", derr)
		return
	}
	for _, w := range watches {
		if err := r.stores.Watches.Delete(ctx, w.ID); err != nil {
			r.log.Warn("reconcile: cannot drop a shared-scope watch", "watch", w.Name, "error", err)
			return
		}
	}
	for _, d := range deployments {
		if err := r.stores.Deployments.Delete(ctx, d.ID); err != nil {
			r.log.Warn("reconcile: cannot drop a shared-scope deployment", "space", d.Space, "error", err)
			return
		}
	}
	if len(watches)+len(deployments) > 0 {
		r.log.Info("reconcile: moved to per-repo scopes", "dropped_watches", len(watches), "dropped_deployments", len(deployments))
	}
	r.purged = true
}
