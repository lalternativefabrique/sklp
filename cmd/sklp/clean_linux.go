package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/lalternativefabrique/sklp/devstack"
	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/internal/plan"
	"github.com/lalternativefabrique/sklp/space"
	"github.com/lalternativefabrique/sklp/space/spacenet"
)

// cleanCommand declares `sklp clean` — the single housekeeping verb. Its flags
// escalate along the axis that actually matters to the operator: the default
// only drops caches a local rebuild recreates, --images adds caches that cost a
// network re-pull, --deep reaches deployment state, and -v destroys data. The
// host toolchain cache (~/.cache/skt) is deliberately out of scope: it is shared
// with skt and re-fetching it is the slowest reconstruction of all.
func cleanCommand() cli.Command {
	return cli.Command{
		Name:  "clean",
		Short: "Free disk space: rebuildable caches + dead runtime state (--images, --deep widen it)",
		Long: "Remove what is rebuildable or already dead: the dev build cache, the CI runner\n" +
			"caches, orphaned processes and residual cgroups. Running deployments are ALWAYS\n" +
			"kept — a live service is not garbage; use `sklp deploy delete` to stop one.\n\n" +
			"--images also clears the OCI image cache (re-pulled next run, and every space's\n" +
			"overlay scratch goes with it). --deep tears down idle spaces WITH their named\n" +
			"volumes — that is persistent data (a Postgres DB, a NATS store), and deleting a\n" +
			"space deletes the space. Every deleted volume is named in the output.\n\n" +
			"Every scope is machine-global: caches are shared by all repositories on this\n" +
			"host, so a clean run from one repo affects the others. The host toolchain cache\n" +
			"(~/.cache/skt) is never touched.\n\n" +
			"Use --dry-run to preview exactly what would be removed without touching anything.",
		Flags: []cli.Flag{
			{Name: "dry-run", Type: cli.BoolFlag, Usage: "print what would be removed, delete nothing"},
			{Name: "images", Type: cli.BoolFlag, Usage: "also clear the OCI image cache (re-pulled next run)"},
			{Name: "deep", Type: cli.BoolFlag, Usage: "also tear down idle spaces and their volumes (running containers are kept)"},
			{Name: "force", Type: cli.BoolFlag, Usage: "skip the confirmation prompt", Aliases: []string{"f"}},
		},
		Run: func(_ context.Context, args cli.Args) int {
			return runClean(os.Stdout, os.Stdin, cleanOpts{
				dryRun: args.Bool("dry-run"),
				images: args.Bool("images"),
				deep:   args.Bool("deep"),
				force:  args.Bool("force"),
			})
		},
	}
}

type cleanOpts struct {
	dryRun bool
	images bool
	deep   bool
	force  bool
}

// needsConfirm gates the prompt on reaching past rebuildable caches. The default
// run only drops what a rebuild recreates and what is already dead, so asking
// there would train the operator to confirm blindly; --deep removes deployment
// state AND its data, which must never happen on a typo.
func (o cleanOpts) needsConfirm() bool { return o.deep }

// runClean plans, previews, confirms when the selection reaches deployment state
// or data, then applies. Returns the process exit code.
func runClean(out io.Writer, in io.Reader, opts cleanOpts) int {
	protected := liveDeploymentCgroups()
	procPlan := spacenet.ReapOrphanProcs(true)
	cgPlan := spacenet.ReapOrphanCgroups(protected, true, nil, nil)
	unownedPlan := spacenet.ReapUnownedCgroups(knownSpacePrefixes(), protected, true, nil, nil)

	fmt.Fprintln(out, "sklp clean — plan:")
	printProcPlan(out, procPlan)
	printCgroupPlan(out, cgPlan)
	printUnownedCgroupPlan(out, notIn(unownedPlan.EmptyRemoved, cgPlan.EmptyRemoved))
	fmt.Fprintf(out, "  CACHES: %s\n", strings.Join(cleanCacheLabels(opts), ", "))
	if opts.deep {
		printIdleSpacePlan(out, idleSpaceNames())
	}
	if kept := notIn(cgPlan.Protected, unownedPlan.EmptyRemoved); len(kept) > 0 {
		sort.Strings(kept)
		fmt.Fprintf(out, "  PROTECTED (kept — live deployment): %s\n", strings.Join(dedup(kept), ", "))
	}

	if opts.dryRun {
		fmt.Fprintln(out, "\nNothing deleted (--dry-run). Re-run without --dry-run to apply.")
		return 0
	}
	if opts.needsConfirm() && !opts.force && !confirmDestructive(out, in) {
		fmt.Fprintln(out, "aborted.")
		return 0
	}

	rc := 0
	spacenet.ReapOrphanProcs(false)
	spacenet.ReapOrphanCgroups(protected, false, killPid, os.Remove)
	// Recompute the owned prefixes at apply time: a space could have been torn
	// down since the plan, and a stale prefix would shield a cgroup that is now
	// unreachable — exactly the leak this pass exists to clear.
	spacenet.ReapUnownedCgroups(knownSpacePrefixes(), protected, false, killPid, os.Remove)

	if opts.deep {
		// reg.Purge keeps every space with a running container, so this never
		// stops a live deployment. It is also the only correct teardown path: a
		// bare RemoveAll on spaces/ would leave overlay mounts behind. Volumes go
		// with their space — deleting a space deletes the space.
		if reg, err := newSpaceRegistry(); err == nil {
			ctx := context.Background()
			volumes := volumesBySpace(ctx, reg)
			if reaped, perr := reg.Purge(ctx); perr != nil {
				fmt.Fprintf(out, "sklp clean: space purge: %v\n", perr)
				rc = 1
			} else if len(reaped) > 0 {
				fmt.Fprintf(out, "torn down %d idle space(s): %s\n", len(reaped), strings.Join(reaped, ", "))
				if names := qualifiedVolumes(volumes, reaped); len(names) > 0 {
					fmt.Fprintf(out, "deleted %d volume(s): %s\n", len(names), strings.Join(names, ", "))
				}
			}
		} else {
			fmt.Fprintf(out, "sklp clean: space registry: %v\n", err)
			rc = 1
		}
	}

	if err := spacenet.RemoveAllAsRoot(devstack.BuildCacheDir()); err != nil {
		fmt.Fprintf(out, "sklp clean: dev build cache: %v\n", err)
		rc = 1
	}
	if opts.images {
		if err := spacenet.RemoveAllAsRoot(plan.NativeImageCacheRoot()); err != nil {
			fmt.Fprintf(out, "sklp clean: image-build cache: %v\n", err)
			rc = 1
		}
	}
	// BuildCaches (runner rootfs builds + ci-toolcache + image-push scratch) is
	// the largest reconstructible cache and never holds deployment state, so it
	// goes in the default run. Volumes are not touched here: they belong to their
	// space and are destroyed by the Purge above, never on their own.
	res, err := space.Clean(space.CleanOpts{Images: opts.images, BuildCaches: true})
	if err != nil {
		fmt.Fprintf(out, "sklp clean: %v\n", err)
		rc = 1
	}
	if len(res.VolumeErrors) > 0 {
		names := make([]string, 0, len(res.VolumeErrors))
		for n := range res.VolumeErrors {
			names = append(names, n)
		}
		sort.Strings(names)
		fmt.Fprintf(out, "sklp clean: could not remove %d volume(s) owned by a container uid: %s\n",
			len(names), strings.Join(names, ", "))
		rc = 1
	}
	if len(res.VolumesSkipped) > 0 {
		sort.Strings(res.VolumesSkipped)
		fmt.Fprintf(out, "kept %d named volume(s) (use -v to remove): %s\n",
			len(res.VolumesSkipped), strings.Join(res.VolumesSkipped, ", "))
	}
	fmt.Fprintln(out, "cleaned.")
	return rc
}

// idleSpaceNames previews what --deep would tear down. It must apply the SAME
// liveness test as runcRegistry.Purge — running-or-created, not "has zero
// containers" — or the plan under-reports: a space whose only container is
// stopped counts as live to a container-count check but is purgeable to the
// registry, and the operator sees it vanish unannounced. Advisory only, since
// the registry re-checks at teardown.
func idleSpaceNames() []string {
	var out []string
	for _, s := range listSpaces() {
		if !spaceHasLiveContainer(s.Name) {
			out = append(out, s.Name)
		}
	}
	sort.Strings(out)
	return out
}

func spaceHasLiveContainer(name string) bool {
	containers, err := space.ListDeployContainers(name)
	if err != nil {
		return false
	}
	for _, c := range containers {
		if statusIsLive(c.Status) {
			return true
		}
	}
	return false
}

// statusIsLive mirrors runcRegistry.hasRunningContainer. Kept as its own
// function so a test pins the two together: they must agree or the plan lies.
func statusIsLive(status string) bool {
	return status == "running" || status == "created"
}

// printIdleSpacePlan names the spaces a purge would reap. Naming them matters:
// a bare "idle spaces are torn down" leaves the operator unable to tell whether
// the one space they care about is in the set.
func printIdleSpacePlan(out io.Writer, names []string) {
	if len(names) == 0 {
		fmt.Fprintln(out, "  IDLE SPACES: none (spaces with a running container are kept)")
		return
	}
	fmt.Fprintf(out, "  IDLE SPACES (%d, spaces with a running container are kept): %s\n",
		len(names), strings.Join(names, ", "))
}

func cleanCacheLabels(o cleanOpts) []string {
	labels := []string{"dev build", "runner+ci-toolcache"}
	if o.images {
		labels = append(labels, "images", "image-build")
	}
	return labels
}
