//go:build linux

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/space"
)

// cacheGCCommand declares `sklp cache gc`. Where `sklp clean` wipes, gc
// reclaims selectively: CI tool caches whose checkout is gone, then the
// least-recently-used ones once the total exceeds a budget. Every CI run does
// this automatically; the verb exists for the buckets predating the origin
// sidecar (never provably dead, so never auto-collected) and to inspect the
// decision with --dry-run before it acts.
func cacheGCCommand() cli.Command {
	return cli.Command{
		Name:  "gc",
		Short: "Reclaim CI tool caches of deleted checkouts (--dry-run to preview)",
		Flags: []cli.Flag{
			{Name: "dry-run", Type: cli.BoolFlag, Usage: "report what would be reclaimed without removing anything"},
			{Name: "max-size", Type: cli.StringFlag, StringDefault: "", Usage: "LRU budget for ci-toolcache, e.g. 12G (0 disables; default 12G)"},
			{Name: "runner-max-size", Type: cli.StringFlag, StringDefault: "", Usage: "LRU budget for built runner rootfs trees, e.g. 8G (0 disables; default 8G)"},
		},
		Run: func(_ context.Context, args cli.Args) int {
			max, err := parseCacheSize(args.String("max-size"))
			if err != nil {
				fmt.Fprintf(os.Stderr, "sklp cache gc: %v\n", err)
				return 2
			}
			if max < 0 {
				max = space.DefaultToolcacheBudget()
			}
			runnerMax, err := parseCacheSize(args.String("runner-max-size"))
			if err != nil {
				fmt.Fprintf(os.Stderr, "sklp cache gc: %v\n", err)
				return 2
			}
			if runnerMax < 0 {
				runnerMax = space.DefaultRunnerBudget()
			}
			dry := args.Bool("dry-run")
			rc := runCacheGC(os.Stdout, os.Stderr, space.ToolcacheGCOpts{
				Orphans: true, MaxBytes: max, DryRun: dry,
			})
			if rrc := runRunnerGC(os.Stdout, os.Stderr, space.RunnerGCOpts{
				Partial: true, MaxBytes: runnerMax, DryRun: dry,
			}); rrc != 0 {
				return rrc
			}
			if irc := runImageGC(os.Stdout, os.Stderr, dry); irc != 0 {
				return irc
			}
			return rc
		},
	}
}

// runCacheGC executes a collection pass and renders it. Split from the verb
// shell so tests capture the output without spawning a subprocess.
func runCacheGC(out, errOut io.Writer, opts space.ToolcacheGCOpts) int {
	res, err := space.CollectToolcache(opts)
	if err != nil {
		fmt.Fprintf(errOut, "sklp cache gc: %v\n", err)
		return 1
	}
	verb := "reclaimed"
	if opts.DryRun {
		verb = "would reclaim"
	}
	for _, b := range res.Removed {
		reason := "checkout gone: " + b.Origin.Repo
		if !b.Known {
			reason = "over budget (no recorded checkout)"
		} else if !b.Orphan {
			reason = "over budget, least recently used"
		}
		fmt.Fprintf(out, "  %-32s %10s  %s\n", b.Name, humanBytes(b.Size), reason)
	}
	fmt.Fprintf(out, "ci-toolcache: %s %s from %d bucket(s), %d kept\n",
		verb, humanBytes(res.ReclaimedFor), len(res.Removed), len(res.Kept))
	if n := len(res.SkippedLive); n > 0 {
		fmt.Fprintf(out, "  %d bucket(s) held by a live space, left in place\n", n)
	}
	if len(res.Errors) > 0 {
		names := make([]string, 0, len(res.Errors))
		for n := range res.Errors {
			names = append(names, n)
		}
		sort.Strings(names)
		fmt.Fprintf(errOut, "sklp cache gc: could not remove %d bucket(s): %s\n", len(names), strings.Join(names, ", "))
		return 1
	}
	return 0
}

// runImageGC executes an image-cache collection pass and renders it. Unlike
// the two budget-driven collectors above, eligibility here is referential and
// generational: an extracted image goes when nothing uses it — no space
// manifest names it, no process holds it as an overlay lower — and it is not
// among the newest few generations of its own repository. There is no size to
// tune, only that per-service count.
func runImageGC(out, errOut io.Writer, dry bool) int {
	res, err := space.ImageGC(dry)
	if err != nil {
		fmt.Fprintf(errOut, "sklp cache gc: %v\n", err)
		return 1
	}
	verb := "reclaimed"
	if dry {
		verb = "would reclaim"
	}
	for _, r := range res.Removed {
		fmt.Fprintf(out, "  %-32s %10s  %s\n", short(r.Digest), humanBytes(r.Size), r.Reason)
	}
	fmt.Fprintf(out, "image cache: %s %s from %d image(s), %d live, %d recent\n",
		verb, humanBytes(res.FreedBytes), len(res.Removed), res.KeptLive, res.KeptRecent)
	if res.EvictedOverKeep > 0 {
		fmt.Fprintf(out, "  %d image(s) past the %d kept per service\n", res.EvictedOverKeep, space.ImageGCKeepPerRepo())
	}
	if res.KeptMounted > 0 {
		fmt.Fprintf(out, "  %d image(s) still mounted by a container, left in place\n", res.KeptMounted)
	}
	return 0
}

// short truncates a digest for display, as image tooling conventionally does.
func short(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}

// runRunnerGC executes a runner-rootfs collection pass and renders it. Split
// from the verb shell so tests capture the output without spawning a
// subprocess.
func runRunnerGC(out, errOut io.Writer, opts space.RunnerGCOpts) int {
	res, err := space.CollectRunner(opts)
	if err != nil {
		fmt.Fprintf(errOut, "sklp cache gc: %v\n", err)
		return 1
	}
	verb := "reclaimed"
	if opts.DryRun {
		verb = "would reclaim"
	}
	for _, r := range res.Removed {
		reason := "over budget, least recently used"
		if !r.Built {
			reason = "partial build, unusable"
		}
		fmt.Fprintf(out, "  %-32s %10s  %s\n", r.Hash, humanBytes(r.Size), reason)
	}
	fmt.Fprintf(out, "runner cache: %s %s from %d rootfs(es), %d kept\n",
		verb, humanBytes(res.ReclaimedFor), len(res.Removed), len(res.Kept))
	if n := len(res.SkippedLive); n > 0 {
		fmt.Fprintf(out, "  %d rootfs(es) in use by a run or space, left in place\n", n)
	}
	if len(res.Errors) > 0 {
		names := make([]string, 0, len(res.Errors))
		for n := range res.Errors {
			names = append(names, n)
		}
		sort.Strings(names)
		fmt.Fprintf(errOut, "sklp cache gc: could not remove %d rootfs(es): %s\n", len(names), strings.Join(names, ", "))
		return 1
	}
	return 0
}
