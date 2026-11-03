package main

import (
	"context"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/internal/containerinit"
)

// pid1Command is the hidden `sklet __init -- <cmd>` verb: the reaping PID 1 a
// deployed service runs under (see space.Service.ReapInit).
func pid1Command() cli.Command {
	return cli.Command{
		Name:    "__init",
		Short:   "Container PID 1: run a command, forward signals, reap orphans (internal)",
		Hidden:  true,
		RawArgs: true,
		Run: func(ctx context.Context, args cli.Args) int {
			return containerinit.Run(args.Passthrough())
		},
	}
}
