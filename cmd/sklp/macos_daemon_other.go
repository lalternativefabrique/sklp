//go:build !(darwin && arm64 && cgo)

package main

import (
	"context"

	"github.com/lalternativefabrique/sklp/internal/cli"
)

// The VM daemon only exists where the native backend is built. The verb stays
// in the tree so the command surface matches across platforms.
func macosDaemonCommand() cli.Command {
	return cli.Command{
		Name:   "__macos-vm",
		Hidden: true,
		Run:    func(_ context.Context, _ cli.Args) int { return 1 },
	}
}
