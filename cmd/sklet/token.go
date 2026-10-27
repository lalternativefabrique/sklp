package main

import (
	"context"
	"fmt"
	"os"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

// tokenCommand declares `sklet token rotate`: the on-node fallback when the
// old token is lost. The running agent picks the new file up on its next
// failed auth, no restart. Record the printed value in `sklp deploy login`.
func tokenCommand() cli.Command {
	return cli.Command{
		Name:  "token",
		Short: "Manage this node's control API Bearer token",
		Sub: []cli.Command{
			{
				Name:  "rotate",
				Short: "Replace the control API token and print the new one; the old one stops working immediately",
				Run: func(ctx context.Context, args cli.Args) int {
					tok, err := control.RotateToken("")
					if err != nil {
						fmt.Fprintln(os.Stderr, "sklet token rotate:", err)
						return 1
					}
					fmt.Println(tok)
					return 0
				},
			},
		},
	}
}
