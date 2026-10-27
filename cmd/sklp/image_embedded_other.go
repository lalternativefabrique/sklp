//go:build !linux

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/lalternativefabrique/sklp/internal/cli"
)

// imageCommand on a non-Linux build: the daemonless OCI builder is Linux-only
// (runc + namespaces), so `sklp image` cannot run here. Declare the same verb
// shape but fail with an actionable message, like the other Linux-only verbs.
func imageCommand() cli.Command {
	return cli.Command{
		Name:    "image",
		Aliases: []string{"img"},
		Short:   "Build + push an OCI image daemonless (sklp image push)",
		Run: func(ctx context.Context, args cli.Args) int {
			fmt.Fprintln(os.Stderr,
				"sklp image: building OCI images requires Linux (the daemonless runc builder); "+
					"this platform is not supported")
			return 2
		},
	}
}
