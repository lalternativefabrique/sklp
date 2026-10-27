package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/nats-io/nats.go/jetstream"

	deployapply "github.com/lalternativefabrique/sklp/reconcile/deployconfig/apply"
	deployconfigdomain "github.com/lalternativefabrique/sklp/reconcile/deployconfig/domain"
	"github.com/lalternativefabrique/sklp/reconcile/deployconfig/nodeapi"
	deployconfigstore "github.com/lalternativefabrique/sklp/reconcile/deployconfig/store"
	gatewaystore "github.com/lalternativefabrique/sklp/reconcile/gateway/store"
	"github.com/lalternativefabrique/sklp/reconcile/gitsource"
	imageupdatestore "github.com/lalternativefabrique/sklp/reconcile/imageupdate/store"
	imagesync "github.com/lalternativefabrique/sklp/reconcile/imageupdate/sync"
	"github.com/lalternativefabrique/sklp/reconcile/source"
)

// bootstrap.go assembles the node's reconciler: the buckets its state lives
// in, the mirrors it pulls from, and the loop that drives them.
//
// It is the one place that names concrete implementations. Everything below it
// takes a port, which is what let the KV adapters be written and tested before
// this file existed.

// Stores are the node's persistence, opened over its embedded JetStream.
type Stores struct {
	Deployments *deployconfigstore.KVRepository
	Nodes       *gatewaystore.KVNodeRepository
	Routes      *gatewaystore.KVRouteRepository
	Watches     *imageupdatestore.KVRepository
}

// OpenStores creates the buckets if they do not exist and binds a repository
// to each. Creating is idempotent, so a node that restarts reuses what it had:
// the state is derived and losing it costs one redundant pass, but there is no
// reason to throw it away on every boot.
func OpenStores(ctx context.Context, js jetstream.JetStream, cipher imageupdatestore.Sealer) (*Stores, error) {
	deployments, err := bucket(ctx, js, deployconfigstore.DeploymentsBucket)
	if err != nil {
		return nil, err
	}
	nodes, err := bucket(ctx, js, gatewaystore.NodesBucket)
	if err != nil {
		return nil, err
	}
	nodeNames, err := bucket(ctx, js, gatewaystore.NodeNamesBucket)
	if err != nil {
		return nil, err
	}
	routes, err := bucket(ctx, js, gatewaystore.RoutesBucket)
	if err != nil {
		return nil, err
	}
	watches, err := bucket(ctx, js, imageupdatestore.WatchesBucket)
	if err != nil {
		return nil, err
	}
	rolls, err := bucket(ctx, js, imageupdatestore.RollsBucket)
	if err != nil {
		return nil, err
	}
	return &Stores{
		Deployments: deployconfigstore.NewKVRepository(deployments),
		Nodes:       gatewaystore.NewKVNodeRepository(nodes, nodeNames),
		Routes:      gatewaystore.NewKVRouteRepository(routes),
		Watches:     imageupdatestore.NewKVRepository(watches, rolls, cipher),
	}, nil
}

func bucket(ctx context.Context, js jetstream.JetStream, name string) (jetstream.KeyValue, error) {
	kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: name})
	if err != nil {
		return nil, fmt.Errorf("open bucket %s: %w", name, err)
	}
	return kv, nil
}

// Mirror is a repo's local clone: it both resolves the branch for the loop and
// reads the declarations out of the commit, which is why one object serves as
// the Target's Source and as the handlers' FileFetcher.
type Mirror struct {
	Target  Target
	Fetcher source.FileFetcher
}

// BuildTargets turns a config into the loop's targets, one bare mirror per
// repo under cacheRoot.
func BuildTargets(cfg *Config, cacheRoot string) ([]Target, error) {
	mirrors, err := BuildMirrors(cfg, cacheRoot)
	if err != nil {
		return nil, err
	}
	targets := make([]Target, 0, len(mirrors))
	for _, m := range mirrors {
		targets = append(targets, m.Target)
	}
	return targets, nil
}

// BuildMirrors is BuildTargets keeping each repo's fetcher, for a caller that
// also has to read files out of the commit it resolved.
func BuildMirrors(cfg *Config, cacheRoot string) ([]Mirror, error) {
	if err := os.MkdirAll(filepath.Join(cacheRoot, "reconcile"), 0o700); err != nil {
		return nil, fmt.Errorf("create mirror root: %w", err)
	}
	mirrors := make([]Mirror, 0, len(cfg.Targets))
	for _, t := range cfg.Targets {
		repo := gitsource.New(t.Repo, MirrorDir(cacheRoot, t.Name))
		mirrors = append(mirrors, Mirror{
			Target:  Target{Repo: t.Name, Branch: t.Branch, Source: repo},
			Fetcher: repo,
		})
	}
	return mirrors, nil
}

// ConfigPath resolves reconcile.yaml beside the node's other sklp config,
// honouring XDG_CONFIG_HOME the way the control credentials already do.
func ConfigPath() string {
	return filepath.Join(configRoot(), "sklp", ConfigName)
}

// CacheRoot resolves where the mirrors live.
func CacheRoot() string {
	if dir := os.Getenv("XDG_CACHE_HOME"); dir != "" {
		return filepath.Join(dir, "sklp")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "sklp-cache")
	}
	return filepath.Join(home, ".cache", "sklp")
}

func configRoot() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "sklp-config")
	}
	return filepath.Join(home, ".config")
}

// Describe reports what a loop is about to do, so an operator starting one
// sees the cadence and the repos rather than a silent process.
func Describe(cfg *Config, log *slog.Logger) {
	if len(cfg.Targets) == 0 {
		log.Info("reconcile: no targets declared, nothing to pull",
			"config", ConfigPath())
		return
	}
	for _, t := range cfg.Targets {
		// t.Repo may carry a credential; only the derived name is logged.
		log.Info("reconcile: watching", "repo", t.Name, "branch", t.Branch)
	}
	log.Info("reconcile: polling", "interval", cfg.Interval.String())
}

// BuildReconcilers assembles what acts on a fetched commit.
//
// Every port here is answered locally: the deployments and watches come from
// this node's KV, the node directory is the node itself, and the applier
// talks to the supervisor over loopback. Nothing reaches core, which is what
// ADR-014 means by a reconciler that runs where it deploys.
//
// secrets may be nil. A deploy.yaml with no ${VAR} reconciles either way, and
// one that references a secret fails naming it rather than sending the literal
// "${VAR}" to a container.
// fetcher reads the declarations out of the mirror the loop keeps for this
// repo, which is the same object the Target already holds.
func BuildReconcilers(stores *Stores, node *LocalNode, fetcher source.FileFetcher, secrets deployconfigdomain.Resolver) []Reconciler {
	scope := NodeScope(node.Name)
	deploy := deployapply.New(
		stores.Deployments,
		fetcher,
		node,
		PlainToken{},
		nodeapi.NewControlApplier(),
		nodeapi.NewControlStateFetcher(),
		secrets,
	)
	watches := imagesync.New(stores.Watches, fetcher, nodeWatchDirectory{node}, PlainToken{})
	return []Reconciler{
		NewDeployReconciler(deploy, scope),
		NewWatchReconciler(watches, scope),
	}
}

// nodeWatchDirectory answers imagesync's node directory with the same rule
// LocalNode applies: this node, under its own name, and nothing else.
type nodeWatchDirectory struct{ node *LocalNode }

func (d nodeWatchDirectory) Credential(ctx context.Context, stackID, nodeName string) (imagesync.NodeAddress, error) {
	addr, err := d.node.Credential(ctx, stackID, nodeName)
	if err != nil {
		return imagesync.NodeAddress{}, translateNotFound(err)
	}
	return imagesync.NodeAddress(addr), nil
}

func (d nodeWatchDirectory) CredentialByID(ctx context.Context, stackID, nodeID string) (imagesync.NodeAddress, error) {
	_, err := d.node.CredentialByID(ctx, stackID, nodeID)
	return imagesync.NodeAddress{}, translateNotFound(err)
}

// translateNotFound re-raises LocalNode's refusal as the sentinel THIS package
// checks. Both are `errors.New("node not found")` in their own package, so
// passing one through where the other is expected compiles and never matches:
// errors.Is compares identity, not text. Nothing in sync tests it today, which
// is exactly why it would be missed the day something does — the same shape as
// the gitsource sentinel that made a missing image-watches.yaml read as a
// failed pass.
func translateNotFound(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, deployapply.ErrNodeNotFound) {
		return fmt.Errorf("%w: %s", imagesync.ErrNodeNotFound, err.Error())
	}
	return err
}
