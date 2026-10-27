package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/space/host"
)

func hostCommand() cli.Command {
	return cli.Command{
		Name:  "host",
		Short: "Inspect this machine as a Space host",
		Sub: []cli.Command{
			{
				Name:  "check",
				Short: "Report what this host lacks to run a Space (exit 1 on a fatal gap)",
				Run: func(ctx context.Context, args cli.Args) int {
					return runHostCheck()
				},
			},
			{
				Name:  "packages",
				Short: "Print the packages every Space host needs, one per line",
				Run: func(ctx context.Context, args cli.Args) int {
					for _, p := range host.Packages {
						fmt.Println(p)
					}
					return 0
				},
			},
		},
	}
}

func runHostCheck() int {
	problems := host.Check(hostCacheRoot())
	if len(problems) == 0 {
		fmt.Println("host ok: this machine can run a Space")
		return 0
	}
	for _, p := range problems {
		fmt.Println(p)
	}
	if host.HasFatal(problems) {
		return 1
	}
	return 0
}

func hostCacheRoot() string {
	if base := os.Getenv("XDG_CACHE_HOME"); base != "" {
		return filepath.Join(base, "sklp")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cache", "sklp")
}
