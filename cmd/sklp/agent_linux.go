//go:build linux

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/space"
)

func agentCommand() cli.Command {
	return cli.Command{
		Name:  "agent",
		Short: "Isolated spaces for AI agents (up)",
		Sub: []cli.Command{{
			Name:  "up",
			Short: "Open a repository's agent space and hold it until stdin closes",
			Flags: []cli.Flag{
				{Name: "repo", Type: cli.StringFlag, StringDefault: ".", Usage: "repository holding .sklp/space.yaml, mounted at /workspace"},
				{Name: "hide", Type: cli.StringSliceFlag, Usage: "a file of the repository to mount empty, repeatable (secret files are hidden by name anyway)"},
				{Name: "mount", Type: cli.StringSliceFlag, Usage: "an extra bind, repeatable: /host/path:/in/space[:ro]"},
			},
			Run: func(ctx context.Context, args cli.Args) int {
				mounts, err := parseSpaceMounts(args.StringSlice("mount"))
				if err != nil {
					fmt.Fprintln(os.Stderr, "sklp agent up:", err)
					return 2
				}
				repo, err := filepath.Abs(args.String("repo"))
				if err != nil {
					fmt.Fprintln(os.Stderr, "sklp agent up:", err)
					return 2
				}
				// The space lives as long as whoever started it keeps stdin
				// open: an agent that dies takes its space with it.
				ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
				defer stop()
				ctx, cancel := context.WithCancel(ctx)
				go func() {
					_, _ = io.Copy(io.Discard, os.Stdin)
					cancel()
				}()
				fmt.Printf("agent space name=%s\n", space.AgentSpaceName(repo))
				if err := space.RunAgentSpace(ctx, space.AgentConfig{RepoPath: repo, Hide: args.StringSlice("hide"), Mounts: mounts, Out: os.Stderr}); err != nil {
					fmt.Fprintln(os.Stderr, "sklp agent up:", err)
					return 1
				}
				return 0
			},
		}},
	}
}
