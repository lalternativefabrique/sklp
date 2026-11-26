package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/lalternativefabrique/sklp/cmd/sklp/internal/sklpmcp"
	"github.com/lalternativefabrique/sklp/internal/cli"
)

func mcpCommand() cli.Command {
	return cli.Command{
		Name:  "mcp",
		Short: "Serve sklp to an AI agent over MCP (stdio): dev stacks, spaces, pipelines, and a node with -context",
		Long: "Starts a Model Context Protocol server on stdin/stdout. It drives this machine's dev stacks (dev_*), spaces (space_*)\n" +
			"and pipelines (run_pipeline, job_*); with -context it also pilots that deploy context's node (deploy_*), with the access\n" +
			"`sklp deploy -context <name>` has. Declare it in the agent's MCP config, e.g. .mcp.json:\n\n" +
			`  {"mcpServers": {"sklp": {"command": "sklp", "args": ["mcp", "-context", "demo"]}}}` + "\n\n" +
			"-read-only serves only the tools that read.",
		Flags: []cli.Flag{
			{Name: "context", Type: cli.StringFlag, Usage: "deploy context whose node the deploy_* tools pilot (without it none are served; the active context is never assumed)"},
			{Name: "read-only", Type: cli.BoolFlag, Usage: "serve only the tools that read"},
		},
		Run: func(ctx context.Context, args cli.Args) int {
			ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stop()
			err := sklpmcp.Run(ctx, sklpmcp.Options{
				Context:  args.String("context"),
				ReadOnly: args.Bool("read-only"),
				Version:  version,
				Warn:     os.Stderr,
			})
			if err != nil && ctx.Err() == nil {
				fmt.Fprintln(os.Stderr, "sklp mcp:", err)
				return 1
			}
			return 0
		},
	}
}
