package main

import (
	"context"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/sklet"
)

// hostCommand declares `sklet host setup`: the one root-gated bootstrap a node
// needs before it can run rootless spaces (uidmap helpers, subuid/subgid grant,
// linger). Run unprivileged it only DIAGNOSES; run under sudo it APPLIES.
//
// It lives on sklet, not sklp: preparing a machine to run containers is a node
// concern, and a VM should not have to install the dev CLI for a one-shot setup.
func hostCommand() cli.Command {
	return cli.Command{
		Name:  "host",
		Short: "Prepare this Linux host to run rootless spaces",
		Sub: []cli.Command{
			{
				Name:  "setup",
				Short: "Check (unprivileged) or apply (run via sudo) the rootless host prerequisites: uidmap, subuid/subgid, linger",
				Run: func(ctx context.Context, args cli.Args) int {
					return sklet.RunHostSetup()
				},
			},
			{
				Name:  "configure",
				Short: "Render the sklet-agent unit from ~/.config/sklp/node.yaml and restart it; re-run after any edit",
				Flags: []cli.Flag{
					{Name: "dry-run", Type: cli.BoolFlag, Usage: "print the unit that would be written, change nothing"},
				},
				Run: func(ctx context.Context, args cli.Args) int {
					return sklet.RunHostConfigure(args.Bool("dry-run"))
				},
			},
		},
	}
}
