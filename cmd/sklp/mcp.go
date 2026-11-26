package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/lalternativefabrique/sklp/cmd/sklp/internal/mcpnode"
	"github.com/lalternativefabrique/sklp/internal/cli"
)

func mcpCommand() cli.Command {
	return cli.Command{
		Name:  "mcp",
		Short: "Serve a deploy context's node to an AI agent over MCP (stdio)",
		Long: "Starts a Model Context Protocol server on stdin/stdout that inspects and deploys to the node of one deploy context,\n" +
			"with the access `sklp deploy -context <name>` has. Declare it in the agent's MCP config, e.g. .mcp.json:\n\n" +
			`  {"mcpServers": {"sklp": {"command": "sklp", "args": ["mcp", "-context", "demo"]}}}` + "\n\n" +
			"-read-only leaves out apply_stack, restart_service, run_job and delete_space.",
		Flags: []cli.Flag{
			{Name: "context", Type: cli.StringFlag, Usage: "deploy context to serve (required; the active context is never assumed)"},
			{Name: "read-only", Type: cli.BoolFlag, Usage: "expose only the tools that read the node"},
		},
		Run: func(ctx context.Context, args cli.Args) int {
			ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stop()
			err := mcpnode.Run(ctx, mcpnode.Options{
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
