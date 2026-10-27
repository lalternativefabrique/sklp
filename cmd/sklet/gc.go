package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/space"
)

func gcCommand() cli.Command {
	return cli.Command{
		Name:  "gc",
		Short: "Remove container images this node no longer runs (the agent also does it hourly and after every apply)",
		Long: "An image survives while a space runs it, while a container still mounts it, or for\n" +
			"24h after it was replaced if it is among the 3 newest of its service. Everything\n" +
			"else goes, with every space's copy of it. --dry-run lists without removing.",
		Flags: []cli.Flag{
			{Name: "dry-run", Type: cli.BoolFlag, Usage: "list what would be removed, remove nothing"},
		},
		Run: func(_ context.Context, args cli.Args) int {
			return runImageGC(os.Stdout, os.Stderr, args.Bool("dry-run"))
		},
	}
}

func runImageGC(out, errOut io.Writer, dry bool) int {
	res, err := space.ImageGC(dry)
	if err != nil {
		fmt.Fprintf(errOut, "sklet gc: %v\n", err)
		return 1
	}
	verb := "freed"
	if dry {
		verb = "would free"
	}
	for _, r := range res.Removed {
		fmt.Fprintf(out, "  %-12s %10s  %s\n", shortDigest(r.Digest), gcBytes(r.Size), r.Reason)
	}
	fmt.Fprintf(out, "sklet gc: %s %s from %d image(s); kept %d running, %d mounted, %d recent\n",
		verb, gcBytes(res.FreedBytes), len(res.Removed), res.KeptLive, res.KeptMounted, res.KeptRecent)
	return 0
}

func shortDigest(d string) string {
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

func gcBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.2f MiB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
