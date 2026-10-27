//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lalternativefabrique/sklp/internal/cli"
)

func deployMeshCommand() cli.Command {
	return cli.Command{
		Name:  "mesh",
		Short: "Inspect the WireGuard mesh as the node sees it (ls | rm <id>)",
		Sub: []cli.Command{
			{
				Name:  "ls",
				Short: "List the mesh peers meshauthority knows",
				Flags: append([]cli.Flag{jsonFlag}, netFlagGroup()...),
				Run: func(ctx context.Context, args cli.Args) int {
					return runDeployMeshLs(ctx, netFlagsFromArgs(args), args.Bool("json"))
				},
			},
			{
				Name:        "rm",
				Short:       "Forget a mesh peer silent for 2 minutes (a rebuilt node's old identity); its mesh IP returns to the pool",
				Flags:       netFlagGroup(),
				Positionals: []cli.Positional{{Name: "id", Required: true}},
				Run: func(ctx context.Context, args cli.Args) int {
					return runDeployMeshRm(ctx, firstArg(args, 0), netFlagsFromArgs(args))
				},
			},
		},
	}
}

func runDeployMeshLs(ctx context.Context, nf *netFlags, asJSON bool) int {
	client, err := nf.dialClient("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy mesh ls:", err)
		return 1
	}
	peers, err := client.MeshPeers(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy mesh ls:", err)
		return 1
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].PrivateIP < peers[j].PrivateIP })
	if asJSON {
		return writeJSON(os.Stdout, "sklp deploy mesh ls", peers)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tMESH IP\tENDPOINT\tLAST SEEN\tSERVICES\tSPACES\tSIGN KEY")
	for _, p := range peers {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", p.ID, p.PrivateIP, p.Endpoint, lastSeen(p.LastSeenAt), p.Services, strings.Join(p.Spaces, ","), p.SignKey)
	}
	return exitOnFlushErr(tw.Flush())
}

func runDeployMeshRm(ctx context.Context, id string, nf *netFlags) int {
	client, err := nf.dialClient("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy mesh rm:", err)
		return 1
	}
	if err := client.DeleteMeshPeer(ctx, id); err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy mesh rm:", err)
		return 1
	}
	fmt.Printf("peer %s forgotten; nodes drop it from wg0 on their next peer refresh\n", id)
	return 0
}

func lastSeen(t *time.Time) string {
	if t == nil {
		return "never"
	}
	d := time.Since(*t).Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	}
	if d < 48*time.Hour {
		return d.Truncate(time.Minute).String() + " ago"
	}
	return t.UTC().Format("2006-01-02")
}

func exitOnFlushErr(err error) int {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
