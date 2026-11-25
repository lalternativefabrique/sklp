//go:build !linux

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/lalternativefabrique/sklp/internal/cli"
)

// agentCommand relays `sklp agent …` into the VM, which mounts the home
// directory at the same path, so -repo and -mount paths mean the same there.
func agentCommand() cli.Command {
	return cli.Command{
		Name:    "agent",
		Short:   "Isolated spaces for AI agents (up); on a Mac they run in the VM",
		RawArgs: true,
		Run: func(context.Context, cli.Args) int {
			if home, err := os.UserHomeDir(); err == nil && !insideDir(home) {
				_ = os.Chdir(home)
			}
			return relayOrNeedsLinux("agent")
		},
	}
}

func insideDir(dir string) bool {
	cwd, err := os.Getwd()
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(dir, cwd)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}
