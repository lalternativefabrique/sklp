//go:build !linux

package main

import (
	"context"
	"fmt"
	"os"
	"runtime"

	"github.com/lalternativefabrique/sklp/internal/cli"
)

// cacheGCCommand keeps `sklp cache gc` in the verb tree outside Linux so help
// and completion stay identical across platforms, but the collection itself
// needs the space runtime that holds the buckets. On a workstation that runs
// its builds in a Linux VM the caches live there, and the VM relay handles the
// invocation before dispatch reaches this stub.
func cacheGCCommand() cli.Command {
	return cli.Command{
		Name:  "gc",
		Short: "Reclaim CI tool caches of deleted checkouts (--dry-run to preview)",
		Flags: []cli.Flag{
			{Name: "dry-run", Type: cli.BoolFlag, Usage: "report what would be reclaimed without removing anything"},
			{Name: "max-size", Type: cli.StringFlag, StringDefault: "", Usage: "LRU budget for ci-toolcache, e.g. 12G (0 disables; default 12G)"},
			{Name: "runner-max-size", Type: cli.StringFlag, StringDefault: "", Usage: "LRU budget for built runner rootfs trees, e.g. 8G (0 disables; default 8G)"},
		},
		Run: func(_ context.Context, _ cli.Args) int {
			fmt.Fprintf(os.Stderr, "sklp cache gc: needs a Linux node (running on %s)\n", runtime.GOOS)
			return 1
		},
	}
}
