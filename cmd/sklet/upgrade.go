package main

import (
	"context"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/sklet"
)

func upgradeCommand() cli.Command {
	return cli.Command{
		Name:  "upgrade",
		Short: "Replace this node's sklet with a published version and restart the agent, rolling back if it does not come up (no sudo once the launcher is installed)",
		Flags: []cli.Flag{
			{Name: "version", Type: cli.StringFlag, Usage: "published version to install (default: latest)"},
			{Name: "dist", Type: cli.StringFlag, Usage: "distribution base URL (default: " + sklet.DefaultDistURL + ")"},
			{Name: "force", Type: cli.BoolFlag, Usage: "reinstall and restart even when already on that version"},
		},
		Run: func(ctx context.Context, args cli.Args) int {
			return sklet.RunUpgrade(ctx, sklet.UpgradeOptions{Version: args.String("version"), Dist: args.String("dist"), Force: args.Bool("force")})
		},
	}
}
