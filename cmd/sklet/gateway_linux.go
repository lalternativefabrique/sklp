//go:build linux

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/spacemanager/mux"
)

// gatewayCommand gives sklet the front door its own units reference. In
// ingress-per-space mode the deploy path writes a systemd unit running
// `<binary> gateway mux`, and on a deploy node that binary IS sklet — a verb
// only sklp carried made the unit exit 2/INVALIDARGUMENT on every restart, so
// the node came up with its services running, its routes declared, and nothing
// listening on the front door.
func gatewayCommand() cli.Command {
	return cli.Command{
		Name:  "gateway",
		Short: "Run this node's front door",
		Sub: []cli.Command{{
			Name:  "mux",
			Short: "Route external traffic to each space's ingress (ingress-per-space front door)",
			Flags: []cli.Flag{
				{Name: "listen", Type: cli.StringFlag, StringDefault: ":9080", Usage: "HTTP front door address (routes by Host header)"},
				{Name: "listen-tls", Type: cli.StringFlag, StringDefault: ":9443", Usage: "HTTPS front door address (routes by TLS SNI, passthrough); empty to disable"},
			},
			Run: func(ctx context.Context, args cli.Args) int {
				if err := mux.Run(ctx, args.String("listen"), args.String("listen-tls"), os.Stdout, os.Stderr); err != nil {
					fmt.Fprintln(os.Stderr, "sklet gateway mux:", err)
					return 1
				}
				return 0
			},
		}},
	}
}
