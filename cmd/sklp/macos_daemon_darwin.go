//go:build darwin && arm64 && cgo

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/internal/limarelay"
)

func init() {
	macosDaemonRun = func() int {
		if err := limarelay.RunDaemon(); err != nil {
			fmt.Fprintf(os.Stderr, "sklp %s: %v\n", "__macos-vm", err)
			return 1
		}
		return 0
	}
	macosVMRunning = limarelay.DaemonRunning
	macosVMStop = limarelay.StopDaemon
	macosVMStart = limarelay.StartDaemon
}

// macosDaemonCommand is the detached process that owns the VM. Hidden: it is
// started by sklp itself, never typed.
func macosDaemonCommand() cli.Command {
	return cli.Command{
		Name:   "__macos-vm",
		Hidden: true,
		Run: func(_ context.Context, _ cli.Args) int {
			if macosDaemonRun == nil {
				return 1
			}
			return macosDaemonRun()
		},
	}
}
