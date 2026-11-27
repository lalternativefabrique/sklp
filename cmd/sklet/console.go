package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
	"github.com/lalternativefabrique/sklp/spacemanager/grants"
)

func consoleCommand() cli.Command {
	return cli.Command{
		Name:  "console",
		Short: "Let a deployed service drive this node's control API from inside its container (a dashboard running on the node it manages)",
		Sub: []cli.Command{
			{
				Name:        "allow",
				Short:       "Grant <space>/<service> a control socket at " + grants.MountPoint + "/" + grants.SocketName + "; restart the service to mount it",
				Positionals: []cli.Positional{{Name: "space/service"}},
				Flags: []cli.Flag{
					{Name: "scope", Type: cli.StringFlag, Usage: "read (spaces, state, manifests, logs, follows, watches), operate (read + follow, unfollow, restart) or admin (operate + apply, delete, exec); never the token, mesh or gateway", StringDefault: "read"},
				},
				Run: func(ctx context.Context, args cli.Args) int {
					space, service, ok := parseGrantTarget(args.Positionals())
					if !ok {
						return 2
					}
					scope, err := grants.ParseScope(args.String("scope"))
					if err != nil {
						fmt.Fprintln(os.Stderr, "sklet console allow:", err)
						return 2
					}
					owner, ok := control.SpaceOwner(space)
					if !ok {
						fmt.Fprintf(os.Stderr, "sklet console allow: space %q is not deployed on this node, or records no owner\n", space)
						return 1
					}
					g := grants.Grant{Space: space, Service: service, Owner: owner, Scope: scope, GrantedAt: time.Now().UTC()}
					if err := grants.Put(g); err != nil {
						fmt.Fprintln(os.Stderr, "sklet console allow:", err)
						return 1
					}
					fmt.Printf("granted %s %s while the space is owned by %s\n", g.Key(), scope, owner)
					fmt.Printf("restart it to mount %s/%s: sklp deploy restart %s -space %s\n", grants.MountPoint, grants.SocketName, service, space)
					return 0
				},
			},
			{
				Name:        "revoke",
				Short:       "Take a grant back; its socket closes within seconds",
				Positionals: []cli.Positional{{Name: "space/service"}},
				Run: func(ctx context.Context, args cli.Args) int {
					space, service, ok := parseGrantTarget(args.Positionals())
					if !ok {
						return 2
					}
					removed, err := grants.Remove(space, service)
					if err != nil {
						fmt.Fprintln(os.Stderr, "sklet console revoke:", err)
						return 1
					}
					if !removed {
						fmt.Fprintf(os.Stderr, "sklet console revoke: no grant for %s/%s\n", space, service)
						return 1
					}
					fmt.Printf("revoked %s/%s\n", space, service)
					return 0
				},
			},
			{
				Name:  "ls",
				Short: "List the services allowed to drive this node",
				Run: func(ctx context.Context, args cli.Args) int {
					list, err := grants.Load()
					if err != nil {
						fmt.Fprintln(os.Stderr, "sklet console ls:", err)
						return 1
					}
					tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
					fmt.Fprintln(tw, "SERVICE\tSCOPE\tOWNER\tGRANTED")
					for _, g := range list {
						fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", g.Key(), g.Scope, g.Owner, g.GrantedAt.Format(time.RFC3339))
					}
					_ = tw.Flush()
					return 0
				},
			},
		},
	}
}

func parseGrantTarget(pos []string) (space, service string, ok bool) {
	if len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "usage: <space>/<service>")
		return "", "", false
	}
	space, service, found := strings.Cut(pos[0], "/")
	if !found || !grants.ValidName(space) || !grants.ValidName(service) {
		fmt.Fprintf(os.Stderr, "%q: want <space>/<service>\n", pos[0])
		return "", "", false
	}
	return space, service, true
}
