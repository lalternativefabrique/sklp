package sklet

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"sync"

	"github.com/lalternativefabrique/sklp/reconcile"
	"github.com/lalternativefabrique/sklp/reconcile/imageupdate/rolling"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

// credentialSpace picks the file the host-wide control credentials live in.
const credentialSpace = "production"

// nodeReconcile runs the node's own reconcile inside the agent: the repos it
// follows, the deploys they declare and the image watches they ask for. It
// starts only on a node that reconciles already, or on its first follow, so a
// node driven by the core runs none of it.
type nodeReconcile struct {
	agentCtx context.Context
	listen   string
	log      *slog.Logger

	mu       sync.Mutex
	embedded *reconcile.Embedded
	stores   *reconcile.Stores
	loop     *reconcile.Loop
	poller   *rolling.Poller
	cancel   context.CancelFunc
}

func startNodeReconcile(ctx context.Context, listen string) *nodeReconcile {
	n := &nodeReconcile{agentCtx: ctx, listen: listen, log: slog.Default()}
	control.SetFollows(n)
	if reconcileConfigured() {
		if err := n.ensure(); err != nil {
			fmt.Fprintf(os.Stderr, "sklet agent: reconcile not started: %v\n", err)
		}
	}
	return n
}

func reconcileConfigured() bool {
	for _, p := range []string{reconcile.ConfigPath(), reconcile.StateDir(reconcile.CacheRoot())} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

func (n *nodeReconcile) ensure() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stores != nil {
		return nil
	}
	cfg := &reconcile.Config{}
	if loaded, err := reconcile.LoadConfig(reconcile.ConfigPath()); err == nil {
		cfg = loaded
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	_, fingerprint, err := control.EnsureServerCert(credentialSpace)
	if err != nil {
		return fmt.Errorf("read control certificate: %w", err)
	}
	node := &reconcile.LocalNode{
		Name:          control.NodeName(),
		ControlURL:    loopbackControlURL(n.listen),
		TokenFunc:     func() (string, error) { return control.EnsureToken(credentialSpace) },
		CAFingerprint: fingerprint,
	}
	embedded, err := reconcile.OpenEmbeddedJetStream(reconcile.CacheRoot())
	if err != nil {
		return err
	}
	sealer, err := reconcile.LoadSealer(reconcile.KeyPath())
	if err != nil {
		embedded.Close()
		return err
	}
	stores, err := reconcile.OpenStores(n.agentCtx, embedded.JS, sealer)
	if err != nil {
		embedded.Close()
		return err
	}
	ctx, cancel := context.WithCancel(n.agentCtx)
	loop := reconcile.NewTargetLoop(reconcile.NewRunner(stores, node, cfg.Targets, reconcile.CacheRoot(), n.log),
		reconcile.Options{Interval: cfg.Interval, Logger: n.log})
	go func() { _ = loop.Run(ctx) }()
	n.embedded, n.stores, n.loop, n.cancel = embedded, stores, loop, cancel
	n.poller = reconcile.StartImageRolling(ctx, stores, 0, node)
	n.log.Info("reconcile: running in the agent", "node", node.Name, "control", node.ControlURL)
	return nil
}

func (n *nodeReconcile) Close() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stores == nil {
		return
	}
	n.cancel()
	n.poller.Stop()
	n.embedded.Close()
	n.stores = nil
}

func (n *nodeReconcile) running() (*reconcile.Stores, *reconcile.Loop) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.stores, n.loop
}

func (n *nodeReconcile) ListFollows(ctx context.Context) (any, error) {
	stores, _ := n.running()
	if stores == nil {
		return []reconcile.Follow{}, nil
	}
	list, err := stores.Follows.List(ctx)
	if list == nil {
		list = []reconcile.Follow{}
	}
	return list, err
}

func (n *nodeReconcile) AddFollow(ctx context.Context, req control.FollowRequest) (any, error) {
	if _, err := reconcile.NormalizeFollow(reconcile.Follow{Repo: req.Repo, Branch: req.Branch, Name: req.Name}); err != nil {
		return nil, fmt.Errorf("%w: %v", control.ErrFollowInvalid, err)
	}
	if err := n.ensure(); err != nil {
		return nil, err
	}
	stores, loop := n.running()
	added, err := stores.Follows.Add(ctx, reconcile.Follow{Repo: req.Repo, Branch: req.Branch, Name: req.Name})
	if errors.Is(err, reconcile.ErrFollowTaken) {
		return nil, fmt.Errorf("%w: %v", control.ErrFollowConflict, err)
	}
	if err != nil {
		return nil, err
	}
	go loop.Pass(n.agentCtx)
	return added, nil
}

func (n *nodeReconcile) RemoveFollow(ctx context.Context, name string) error {
	stores, _ := n.running()
	if stores == nil {
		return fmt.Errorf("%w: %s", control.ErrFollowMissing, name)
	}
	err := stores.Follows.Remove(ctx, name)
	if errors.Is(err, reconcile.ErrFollowNotFound) {
		return fmt.Errorf("%w: %v", control.ErrFollowMissing, err)
	}
	return err
}

// loopbackControlURL answers where the agent's own API is reachable from the
// agent: loopback when it listens on every address, else the address it was
// narrowed to (a private origin binds its mesh IP only).
func loopbackControlURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "https://127.0.0.1:6464"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "https://" + net.JoinHostPort(host, port)
}
