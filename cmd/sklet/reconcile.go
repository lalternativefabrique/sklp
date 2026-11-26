package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"

	"golang.org/x/sys/unix"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/reconcile"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

// reconcile.go wires `sklet reconcile`: the node's pull loop (ADR-014).
//
// It reads the repos this node deploys from out of reconcile.yaml, mirrors
// them, and re-applies whatever a new commit declares. Nothing pushes to the
// node — that is the point: a node behind NAT reconciles like one with a
// public address, because it only needs Git outbound.

func reconcileCommand() cli.Command {
	return cli.Command{
		Name:  "reconcile",
		Short: "Pull the repos this node deploys from and apply what changed",
		Long: "Poll every repo declared in reconcile.yaml and reconcile the ones whose branch\n" +
			"moved. The node reaches Git outbound and nothing reaches the node, so one behind\n" +
			"NAT works the same as one with a public address — a push is applied within the\n" +
			"poll interval rather than in seconds, which is the trade.\n\n" +
			"The image watches those repos declare are polled too: a newer tag in a\n" +
			"watched registry is rolled onto this node with no core involved.\n\n" +
			"reconcile.yaml lives beside the control credentials (XDG_CONFIG_HOME/sklp).\n" +
			"With no file, or no targets in it, this node pulls nothing and only serves what\n" +
			"an operator pushed with `sklp deploy`.",
		Flags: []cli.Flag{
			{Name: "config", Type: cli.StringFlag, Usage: "path to reconcile.yaml (default: XDG_CONFIG_HOME/sklp/reconcile.yaml)"},
			{Name: "once", Type: cli.BoolFlag, Usage: "run a single pass and exit, instead of polling — for a manual reconcile or a check"},
			{Name: "space", Type: cli.StringFlag, StringDefault: "production", Usage: "space whose control credentials this node authenticates with (they are host-wide; the name only picks the file)"},
			{Name: "control-url", Type: cli.StringFlag, StringDefault: "https://127.0.0.1:6464", Usage: "this node's control API, which the reconciler applies against over loopback"},
		},
		Run: func(ctx context.Context, args cli.Args) int {
			path := args.String("config")
			if path == "" {
				path = reconcile.ConfigPath()
			}
			cfg, err := reconcile.LoadConfig(path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "sklet reconcile: %v\n", err)
				return 1
			}

			log := slog.Default()
			reconcile.Describe(cfg, log)
			node, err := localNode(args.String("space"), args.String("control-url"))
			if err != nil {
				fmt.Fprintf(os.Stderr, "sklet reconcile: %v\n", err)
				return 1
			}
			log.Info("reconcile: this node", "name", node.Name, "control", node.ControlURL)

			embedded, err := reconcile.OpenEmbeddedJetStream(reconcile.CacheRoot())
			if err != nil {
				fmt.Fprintf(os.Stderr, "sklet reconcile: %v\n", err)
				return 1
			}
			defer embedded.Close()

			sealer, err := reconcile.LoadSealer(reconcile.KeyPath())
			if err != nil {
				fmt.Fprintf(os.Stderr, "sklet reconcile: %v\n", err)
				return 1
			}
			stores, err := reconcile.OpenStores(ctx, embedded.JS, sealer)
			if err != nil {
				fmt.Fprintf(os.Stderr, "sklet reconcile: %v\n", err)
				return 1
			}

			loop := reconcile.NewTargetLoop(reconcile.NewRunner(stores, node, cfg.Targets, reconcile.CacheRoot(), log), reconcile.Options{
				Interval: cfg.Interval,
				Logger:   log,
			})

			if args.Bool("once") {
				loop.Pass(ctx)
				return 0
			}

			// SIGHUP is ignored for the same reason the agent ignores it: a
			// hijacked exec session hanging up must not stop reconciling.
			signal.Ignore(unix.SIGHUP)
			runCtx, stop := signal.NotifyContext(ctx, unix.SIGINT, unix.SIGTERM)
			defer stop()

			poller := reconcile.StartImageRolling(runCtx, stores, 0, node)
			defer poller.Stop()

			if err := loop.Run(runCtx); err != nil {
				fmt.Fprintf(os.Stderr, "sklet reconcile: %v\n", err)
				return 1
			}
			return 0
		},
	}
}

// localNode reads this node's identity and its own control credentials.
//
// The token and the fingerprint are the ones the control API itself serves
// with, read from the same files it wrote — so the reconciler authenticates to
// its own supervisor exactly as a remote `sklp deploy` would, rather than
// carrying a second credential that could drift from the first.
func localNode(space, controlURL string) (*reconcile.LocalNode, error) {
	name := control.NodeName()
	if name == "" {
		return nil, fmt.Errorf("this host has no readable hostname; a node is identified by it")
	}
	token, err := control.EnsureToken(space)
	if err != nil {
		return nil, fmt.Errorf("read control token: %w", err)
	}
	_, fingerprint, err := control.EnsureServerCert(space)
	if err != nil {
		return nil, fmt.Errorf("read control certificate: %w", err)
	}
	return &reconcile.LocalNode{
		Name:          name,
		ControlURL:    controlURL,
		Token:         token,
		CAFingerprint: fingerprint,
	}, nil
}
