//go:build !linux

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/lalternativefabrique/sklp/internal/cli"
)

func agentCommand() cli.Command {
	return cli.Command{
		Name:  "agent",
		Short: "Isolated spaces for AI agents (Linux; macOS through the VM is not wired yet)",
		Run: func(context.Context, cli.Args) int {
			fmt.Fprintln(os.Stderr, "sklp agent: agent spaces run on Linux only for now")
			return 1
		},
	}
}
