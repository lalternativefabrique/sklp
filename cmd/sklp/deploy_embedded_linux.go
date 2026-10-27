// deploy_embedded.go is the CLIENT half of `sklp deploy`: everything a
// workstation does AGAINST a node over the control API (ls / get / logs / exec /
// restart / delete / login / context / token / fingerprint).
//
// The node-side half — the supervisor loop, the __supervise daemon, the local
// launch modes and the empty-agent mode — lives in the sklet binary. The split
// is by execution role: sklp talks to a node, sklet IS a node. A production VM
// therefore never carries the dev/CI/publish surface.
//
// Applying a manifest locally is `sklet apply <stack>`; applying it to a remote
// node stays `sklp deploy <stack>` (routed ahead of dispatch by the --server /
// --context shims in main).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/internal/layout"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

// deployCommand declares the `sklp deploy` verb tree on the façade. The bare
// group Run applies the local manifest to the ACTIVE CONTEXT's node; the
// sub-verbs are thin glue over the control API. The SSH (--host) and network
// (--server) remote shims run AHEAD of dispatch in main, so this tree only ever
// sees the fall-through case.
func deployCommand() cli.Command {
	return cli.Command{
		Name:  "deploy",
		Short: "Pilot a node: apply .sklp/<stack>/deploy.yaml to it and inspect what runs",
		Flags: []cli.Flag{
			{Name: "space", Type: cli.StringFlag, StringDefault: "production", Usage: "space to deploy (named or ephemeral)"},
			{Name: "repo", Type: cli.StringFlag, StringDefault: ".", Usage: "repository root the deploy file is relative to"},
			{Name: "force", Type: cli.BoolFlag, Usage: "adopt a space already deployed by a DIFFERENT stack (else the cross-stack re-apply is refused to avoid reconciling away the owner's services)"},
			{Name: "local", Type: cli.BoolFlag, Usage: "force the LOCAL socket transport even when the active context is remote"},
			{Name: "context", Type: cli.StringFlag, Usage: "deploy context to target (required for a remote apply: the active context is never used implicitly)"},
			{Name: "server", Aliases: []string{"addr"}, Type: cli.StringFlag, Usage: "remote supervisor control API (https://host:9443); when set, apply over the network instead of the local socket"},
			{Name: "token", Type: cli.StringFlag, Usage: "Bearer token for --server (prefer the context/SKLP_TOKEN)"},
			{Name: "token-file", Type: cli.StringFlag, Usage: "file holding the Bearer token for --server"},
			{Name: "ca-fingerprint", Type: cli.StringFlag, Usage: "pin the server certificate sha256 for --server"},
			{Name: "insecure", Type: cli.BoolFlag, Usage: "skip TLS verification for --server (LAN bootstrap only)"},
		},
		Positionals: []cli.Positional{{Name: "stack"}},
		Run: func(ctx context.Context, args cli.Args) int {
			return runDeployClient(firstArg(args, 0), args.String("repo"))
		},
		Sub: []cli.Command{
			deployApplyCommand(),
			deployLsCommand(),
			deployDataStoresCommand(),
			deployRestoresCommand(),
			deployGetCommand(),
			deployLogsCommand(),
			deployDeleteCommand(),
			deployExecCommand(),
			deployRestartCommand(),
			deployLoginCommand(),
			deployContextCommand(),
			deployTokenCommand(),
			deployFingerprintCommand(),
			deployMeshCommand(),
		},
	}
}

// runDeployClient is the fall-through for a bare `sklp deploy [stack]` that the
// remote shims in main did NOT handle — i.e. no --server, no --host, and no
// remote active context. There is nothing left for the client to do: applying a
// manifest to THIS machine is the node's job, so point at sklet rather than
// silently doing nothing.
//
// A bare `sklp deploy` with no stack still prints the catalogue, so the verb
// stays discoverable.
func runDeployClient(stack, repo string) int {
	lay := layout.New(repo)

	if stack == "" {
		printDeployCatalogue(lay)
		return 0
	}

	fmt.Fprintf(os.Stderr, "sklp deploy: no remote target for stack %q.\n\n", stack)
	fmt.Fprintln(os.Stderr, "`sklp deploy` pilots a REMOTE node. Pick one:")
	fmt.Fprintln(os.Stderr, "  • target a node:      sklp deploy "+stack+" --server https://host:6464 --token <T>")
	fmt.Fprintln(os.Stderr, "  • or save a context:  sklp deploy login --context prod --server ... && sklp deploy context use prod")
	fmt.Fprintln(os.Stderr, "  • deploy on THIS machine instead:  sklet apply "+stack)
	return 2
}

// printDeployCatalogue is the bare `sklp deploy` output: the sub-command list
// (from the declared tree, so it never drifts) followed by the deploy-capable
// stacks. Discovery surface for someone who just typed `sklp deploy` + Enter.
func printDeployCatalogue(lay layout.Layout) {
	w := os.Stdout
	fmt.Fprintln(w, "Usage: sklp deploy <subcommand> [flags]")
	fmt.Fprintln(w, "       sklp deploy <stack> --server <url>   # apply a stack to a remote node")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Subcommands:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range deployCommand().Sub {
		if c.Hidden {
			continue
		}
		fmt.Fprintf(tw, "  %s\t%s\n", c.Name, c.Short)
	}
	_ = tw.Flush()

	if stacks, err := lay.StacksFor("deploy"); err == nil && len(stacks) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Deployable stacks:")
		for _, s := range stacks {
			fmt.Fprintf(w, "  %s\n", s)
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Apply one to a remote node with `sklp deploy <stack> --server <url>`,")
		fmt.Fprintln(w, "or on THIS machine with `sklet apply <stack>`.")
	}
}

// deployDeleteCommand declares `sklp deploy delete`, the canonical verb that
// destroys a deployed space's runtime (stops containers, removes the
// spaces/<space>/ artifacts). It does NOT touch the global control identity
// under ~/.config/sklp/control/. The legacy `down` verb is a deprecated alias.
func deployDeleteCommand() cli.Command {
	return cli.Command{
		Name:  "delete",
		Short: "Delete a deployed space's runtime (stop containers, remove its artifacts)",
		Flags: append([]cli.Flag{
			{Name: "file", Type: cli.StringFlag, Usage: "explicit path to deploy.yaml (overrides the <stack> layout resolution)"},
			{Name: "space", Type: cli.StringFlag, StringDefault: "production", Usage: "space to delete"},
			{Name: "repo", Type: cli.StringFlag, StringDefault: ".", Usage: "repository root the deploy file is relative to"},
			{Name: "v", Type: cli.BoolFlag, Usage: "also destroy the space's named volumes — the data goes with it, irreversibly"},
		}, netFlagGroup()...),
		Positionals: []cli.Positional{{Name: "stack"}},
		Run: func(ctx context.Context, args cli.Args) int {
			return runDownSubcommand(firstArg(args, 0), args.String("file"), args.String("space"), args.String("repo"), args.Bool("v"), netFlagsFromArgs(args))
		},
	}
}

// runDownSubcommand backs `sklp deploy delete`. It resolves the same manifest the
// deploy path uses (so the id-based teardown reaches the right containers) and
// delegates to the node-side teardown. Returns the process exit code.
func runDownSubcommand(stack, file, space, repo string, volumes bool, nf *netFlags) int {
	// Remote: a `down` over --server can only ask the supervisor to stop itself
	// (POST /v1/down) — the local id-based teardown / artifact cleanup are
	// meaningless on this machine. The server tears its own space down.
	if nf.remote() {
		return runDownRemote(nf, space, volumes)
	}

	path, err := resolveDeployFile(stack, file, repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy delete:", err)
		return 2
	}

	// Local teardown: acts on THIS host's runc state and config dir. Same path a
	// node runs from inside its own supervisor.
	if err := deployDownLocal(path, space, volumes, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy delete:", err)
		return 1
	}
	return 0
}

// resolveDeployFile finds the deploy.yaml to act on, in priority order:
//
//  1. an explicit --file (made repo-relative when not absolute) — the escape
//     hatch, wins over everything;
//  2. a <stack> positional — resolved to .sklp/<stack>/deploy.yaml via the same
//     layout the deploy up flow uses, so `delete` and up agree on the path;
//  3. legacy fallback to the flat .sklp/deploy.yaml under repo (back-compat for
//     callers from before the per-stack layout).
func resolveDeployFile(stack, file, repo string) (string, error) {
	if file != "" {
		if filepath.IsAbs(file) {
			return file, nil
		}
		return filepath.Join(repo, file), nil
	}
	if stack != "" {
		return layout.New(repo).Resolve(stack, "deploy")
	}
	return filepath.Join(repo, ".sklp", "deploy.yaml"), nil
}

// runDownRemote asks a REMOTE supervisor to tear its space down over the
// network control API (POST /v1/down). It cannot run the local teardown/cleanup
// (those act on this host's runc state / config dir), so it only relays the stop.
func runDownRemote(nf *netFlags, space string, volumes bool) int {
	client, err := nf.dialClient(space)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy delete:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := deleteRemote(ctx, client, volumes); err != nil {
		reportControlErr("sklp deploy delete", space, err)
		return 1
	}
	fmt.Println(deleteAck(space, volumes))
	return 0
}

// deleteRemote picks the teardown the operator asked for: -v destroys the
// named volumes with the space, without them the data outlives it.
func deleteRemote(ctx context.Context, client *control.Client, volumes bool) error {
	if volumes {
		return client.DownAndDelete(ctx)
	}
	return client.DownAndPurge(ctx)
}

// deleteAck reports what was actually asked for: "deleted" alone has meant a
// space that came straight back, since its kept volumes held the space dir —
// and the inventory lists spaces by directory name.
func deleteAck(space string, volumes bool) string {
	if volumes {
		return fmt.Sprintf("space %q: delete requested on remote supervisor (named volumes destroyed)", space)
	}
	return fmt.Sprintf("space %q: delete requested on remote supervisor (named volumes kept — pass -v to destroy them)", space)
}

// liveSpaceOwner returns the stack that owns the live space, read from the
// persisted manifest. ok=false when no manifest exists, it cannot be read, or it
// predates the Stack field (empty). Kept here for the client-side guards that
// report on a locally-visible space.
func liveSpaceOwner(space string) (stack string, ok bool) {
	_, meta, err := control.LoadManifest(space)
	if err != nil || meta.Stack == "" {
		return "", false
	}
	return meta.Stack, true
}

// errStackUnknown is returned by the client paths when a stack positional does
// not resolve, so callers can distinguish it from a transport failure.
var errStackUnknown = errors.New("unknown stack")

// stackHint formats the "available stacks" tail shared by the client errors.
func stackHint(lay layout.Layout) string {
	stacks, err := lay.StacksFor("deploy")
	if err != nil || len(stacks) == 0 {
		return ""
	}
	return "  available: " + strings.Join(stacks, ", ")
}

// deployApplyCommand is the explicit form of the bare group Run: main rewrites
// `deploy apply <stack>` to `deploy <stack>` before dispatch, so this entry
// exists for the catalogue, completion and the fall-through.
func deployApplyCommand() cli.Command {
	return cli.Command{
		Name:  "apply",
		Short: "Apply .sklp/<stack>/deploy.yaml to the node named by -context (required; no stack: list the choices)",
		Flags: []cli.Flag{
			{Name: "space", Type: cli.StringFlag, StringDefault: "production", Usage: "space to deploy (named or ephemeral)"},
			{Name: "repo", Type: cli.StringFlag, StringDefault: ".", Usage: "repository root the deploy file is relative to"},
			{Name: "force", Type: cli.BoolFlag, Usage: "adopt a space already deployed by a DIFFERENT stack"},
			{Name: "local", Type: cli.BoolFlag, Usage: "force the LOCAL socket transport even when the active context is remote"},
			{Name: "context", Type: cli.StringFlag, Usage: "deploy context to target (required for a remote apply: the active context is never used implicitly)"},
			{Name: "server", Aliases: []string{"addr"}, Type: cli.StringFlag, Usage: "remote supervisor control API (https://host:9443)"},
			{Name: "token", Type: cli.StringFlag, Usage: "Bearer token for --server (prefer the context/SKLP_TOKEN)"},
			{Name: "token-file", Type: cli.StringFlag, Usage: "file holding the Bearer token for --server"},
			{Name: "ca-fingerprint", Type: cli.StringFlag, Usage: "pin the server certificate sha256 for --server"},
			{Name: "insecure", Type: cli.BoolFlag, Usage: "skip TLS verification for --server (LAN bootstrap only)"},
		},
		Positionals: []cli.Positional{{Name: "stack"}},
		Run: func(ctx context.Context, args cli.Args) int {
			return runDeployClient(firstArg(args, 0), args.String("repo"))
		},
	}
}
