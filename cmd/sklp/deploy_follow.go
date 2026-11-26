package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

func deployFollowCommand() cli.Command {
	return cli.Command{
		Name:        "follow",
		Short:       "Have the node follow a public repo: it applies the repo's deploy-root and rolls its image watches itself",
		Positionals: []cli.Positional{{Name: "repo", Required: true}},
		Flags: append([]cli.Flag{
			{Name: "branch", Type: cli.StringFlag, Usage: "branch to follow (default main)"},
			{Name: "name", Type: cli.StringFlag, Usage: "name the node keeps the repo under (default: the repo's last path element)"},
		}, netFlagGroup()...),
		Run: func(ctx context.Context, args cli.Args) int {
			return runDeployFollow(ctx, netFlagsFromArgs(args), control.FollowRequest{
				Repo: firstArg(args, 0), Branch: args.String("branch"), Name: args.String("name"),
			})
		},
	}
}

func deployFollowsCommand() cli.Command {
	return cli.Command{
		Name:  "follows",
		Short: "List the repos the node follows",
		Flags: append([]cli.Flag{jsonFlag}, netFlagGroup()...),
		Run: func(ctx context.Context, args cli.Args) int {
			return runDeployFollows(ctx, netFlagsFromArgs(args), args.Bool("json"))
		},
	}
}

func deployUnfollowCommand() cli.Command {
	return cli.Command{
		Name:        "unfollow",
		Short:       "Stop following a repo (its spaces keep running until deleted)",
		Positionals: []cli.Positional{{Name: "name", Required: true}},
		Flags:       netFlagGroup(),
		Run: func(ctx context.Context, args cli.Args) int {
			return runDeployUnfollow(ctx, netFlagsFromArgs(args), firstArg(args, 0))
		},
	}
}

func runDeployFollow(ctx context.Context, nf *netFlags, req control.FollowRequest) int {
	if req.Repo == "" {
		fmt.Fprintln(os.Stderr, "usage: sklp deploy follow <https://repo> [-branch B] [-name N] -context <node>")
		return 2
	}
	client, err := nf.dialClient("production")
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy follow:", err)
		return 1
	}
	f, err := client.Follow(ctx, req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy follow:", err)
		return 1
	}
	fmt.Printf("following %s (%s) as %q; the node applies it within a minute\n", f.Repo, f.Branch, f.Name)
	return 0
}

func runDeployFollows(ctx context.Context, nf *netFlags, asJSON bool) int {
	client, err := nf.dialClient("production")
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy follows:", err)
		return 1
	}
	res, err := client.Follows(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy follows:", err)
		return 1
	}
	if asJSON {
		return writeJSON(os.Stdout, "sklp deploy follows", res.Follows)
	}
	if len(res.Follows) == 0 {
		fmt.Printf("node %s follows no repo\n", res.Node)
		return 0
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tREPO\tBRANCH\tSINCE")
	for _, f := range res.Follows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", f.Name, f.Repo, f.Branch, f.AddedAt.Format("2006-01-02 15:04"))
	}
	_ = w.Flush()
	return 0
}

func runDeployUnfollow(ctx context.Context, nf *netFlags, name string) int {
	if name == "" {
		fmt.Fprintln(os.Stderr, "usage: sklp deploy unfollow <name> -context <node>")
		return 2
	}
	client, err := nf.dialClient("production")
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy unfollow:", err)
		return 1
	}
	if err := client.Unfollow(ctx, name); err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy unfollow:", err)
		return 1
	}
	fmt.Printf("no longer following %q; its spaces keep running until deleted\n", name)
	return 0
}
