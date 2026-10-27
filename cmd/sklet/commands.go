package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/internal/layout"
	"github.com/lalternativefabrique/sklp/sklet"
	"github.com/lalternativefabrique/sklp/spacemanager"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
	"github.com/lalternativefabrique/sklp/spacemanager/deployer"
	// Blank import: register the traefik gateway driver so spacemanager.OpenGateway(
	// "traefik") resolves at the CLI edge WITHOUT package sklet importing any
	// traefik code (the gateway-agnosticism invariant). This is the single place
	// the binary is wired to a concrete front-door driver.
	_ "github.com/lalternativefabrique/sklp/spacemanager/gateway/traefik"
)

// rootCommand assembles the sklet verb tree. It is deliberately small: this
// binary applies a manifest to THIS node and supervises it. Everything a
// workstation does against a node (ls/logs/exec/context/login) lives in sklp.
func rootCommand() cli.Command {
	return cli.Command{
		Name:  "sklet",
		Short: "node-side deploy agent",
		Long:  usage,
		Sub: []cli.Command{
			applyCommand(),
			agentCommand(),
			reconcileCommand(),
			hostCommand(),
			tokenCommand(),
			gatewayCommand(),
			spaceCommand(),
			versionCommand(),
			gcCommand(),
			{
				// Internal: the node runs it inside a restore job, configured
				// through the environment it composes from the production space.
				Name:   "drill",
				Hidden: true,
				Run: func(ctx context.Context, args cli.Args) int {
					return sklet.RunDrill()
				},
			},
			upgradeCommand(),
			{
				Name:   "__doc",
				Hidden: true,
				Run: func(ctx context.Context, args cli.Args) int {
					if err := cli.WriteDoc(os.Stdout, rootCommand()); err != nil {
						fmt.Fprintln(os.Stderr, "sklet __doc:", err)
						return 1
					}
					return 0
				},
			},
			{
				// Internal: started by systemd OR the daemonless setsid detach, never
				// by a human (the __ prefix). Keeps its own RawArgs flag parsing.
				Name:    "__supervise",
				Hidden:  true,
				RawArgs: true,
				Run: func(ctx context.Context, args cli.Args) int {
					sklet.RunSupervise(args.Passthrough())
					return 0
				},
			},
		},
	}
}

// applyCommand declares `sklet apply [stack]`: resolve .sklp/<stack>/deploy.yaml
// and reconcile it on this node. A bare `sklet apply` lists the deployable stacks.
func applyCommand() cli.Command {
	return cli.Command{
		Name:  "apply",
		Short: "Apply a stack from .sklp/<stack>/deploy.yaml on this node",
		Flags: []cli.Flag{
			{Name: "space", Type: cli.StringFlag, StringDefault: "production", Usage: "space to deploy (named or ephemeral)"},
			{Name: "repo", Type: cli.StringFlag, StringDefault: ".", Usage: "repository root the deploy file is relative to"},
			{Name: "foreground", Type: cli.BoolFlag, Usage: "run the supervisor in the foreground (Ctrl-C to stop) instead of detaching (systemd --user, or setsid when unavailable); debug use"},
			{Name: "listen", Type: cli.StringFlag, Usage: "also expose the control API on this TCP address (e.g. :9443) over TLS + a Bearer token, for remote piloting without SSH; the local socket is always served"},
			{Name: "force", Type: cli.BoolFlag, Usage: "adopt a space already deployed by a DIFFERENT stack (else the cross-stack re-apply is refused to avoid reconciling away the owner's services)"},
			{Name: "gateway-mode", Type: cli.StringFlag, StringDefault: spacemanager.GatewayModeSingleton, Usage: "front door for this LOCAL node: per-space (native mux on :9080/:9443) or singleton (legacy traefik); a control-plane node instead uses `sklet agent --gateway-mode`"},
		},
		Positionals: []cli.Positional{{Name: "stack"}},
		Run: func(ctx context.Context, args cli.Args) int {
			return runApply(applyParams{
				stack:       firstArg(args, 0),
				space:       args.String("space"),
				repo:        args.String("repo"),
				foreground:  args.Bool("foreground"),
				listen:      args.String("listen"),
				force:       args.Bool("force"),
				gatewayMode: args.String("gateway-mode"),
			})
		},
	}
}

// applyParams holds the parsed `sklet apply [stack]` flags.
type applyParams struct {
	stack       string
	space       string
	repo        string
	foreground  bool
	listen      string
	force       bool
	gatewayMode string
}

// firstArg returns the nth positional or "" when absent.
func firstArg(args cli.Args, n int) string {
	pos := args.Positionals()
	if n < len(pos) {
		return pos[n]
	}
	return ""
}

// runApply loads .sklp/<stack>/deploy.yaml, resolves the requested space to a
// runnable service set, then launches the supervisor for it (detached under
// systemd --user when available, else a setsid daemon; foreground with
// --foreground). Teardown is `sklp deploy delete`. Returns the process exit code.
func runApply(p applyParams) int {
	lay := layout.New(p.repo)

	// No positional <stack>: catalogue mode, so a bare `sklet apply` is discoverable.
	if p.stack == "" {
		printApplyCatalogue(lay)
		return 0
	}

	path, err := lay.Resolve(p.stack, "deploy")
	if err != nil {
		switch {
		case errors.Is(err, layout.ErrStackNotFound):
			fmt.Fprintf(os.Stderr, "sklet apply: unknown stack %q\n", p.stack)
			if stacks, _ := lay.StacksFor("deploy"); len(stacks) > 0 {
				fmt.Fprintf(os.Stderr, "  available: %s\n", strings.Join(stacks, ", "))
			}
		default: // ErrNoContract: the message already lists the stack's verbs.
			fmt.Fprintf(os.Stderr, "sklet apply: %v\n", err)
		}
		return 2
	}

	// Fail early with an actionable hint if a hard rootless prerequisite is
	// missing, instead of the cryptic runc "nsexec: mapping tool not present"
	// surfaced deep in service launch. No-op once the host is set up.
	if msg := sklet.HostPreflight(); msg != "" {
		fmt.Fprintln(os.Stderr, "sklet apply:", msg)
		return 1
	}

	if err := spacemanager.ValidateGatewayMode(p.gatewayMode); err != nil {
		fmt.Fprintln(os.Stderr, "sklet apply: --gateway-mode:", err)
		return 2
	}

	// The supervisor-bound launch steps live in package sklet (shared with
	// __supervise); inject them into the use case. The Launch hook captures
	// foreground (no UpConfig field for a debug-only flag).
	deployer.SetHooks(applyHooks(p.foreground))

	if err := deployer.Up(context.Background(), deployer.UpConfig{
		Stack:       p.stack,
		File:        path,
		Space:       p.space,
		Repo:        p.repo,
		Listen:      p.listen,
		Force:       p.force,
		GatewayMode: p.gatewayMode,
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "sklet apply:", err)
		return 1
	}
	return 0
}

// hookWriter mirrors deployer.Hooks' (unexported) writer alias so the adapter
// func literals are assignable to the hook fields. Same shape as io.Writer.
type hookWriter = interface {
	Write(p []byte) (int, error)
}

// applyHooks adapts the node-resident supervisor steps to deployer.Hooks. Each
// hook returns an error (no os.Exit) and writes to the injected out/errOut.
func applyHooks(foreground bool) deployer.Hooks {
	return deployer.Hooks{
		TryReapply: sklet.TryReapply,
		Launch: func(cfg *spacemanager.DeployConfig, services []spacemanager.Service, meta spacemanager.SpaceMeta, listen string, out, errOut hookWriter) error {
			switch sklet.ChooseMode(foreground, control.SystemdAvailable()) {
			case sklet.ModeDetached:
				return sklet.DeployDetached(cfg, services, meta)
			case sklet.ModeDetachedNoSystemd:
				// systemd --user unavailable (CI, non-login ssh, container): detach the
				// supervisor ourselves (setsid + log redirection) so the terminal is
				// freed and the space survives the shell closing — same __supervise.
				return sklet.DeployDetachedNoSystemd(cfg, services, meta)
			default: // ModeForeground (explicit --foreground)
				return sklet.DeployForeground(cfg, services, meta, listen)
			}
		},
	}
}

// printApplyCatalogue is the bare `sklet apply` output: the deploy-capable
// stacks on this repo. Discovery surface for someone who just typed it + Enter.
func printApplyCatalogue(lay layout.Layout) {
	w := os.Stdout
	fmt.Fprintln(w, "Usage: sklet apply <stack> [flags]")
	fmt.Fprintln(w)

	stacks, err := lay.StacksFor("deploy")
	if err != nil || len(stacks) == 0 {
		fmt.Fprintln(w, "No deployable stacks found (expected .sklp/<stack>/deploy.yaml).")
		return
	}
	fmt.Fprintln(w, "Deployable stacks (run with `sklet apply <stack>`):")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, s := range stacks {
		fmt.Fprintf(tw, "  %s\n", s)
	}
	_ = tw.Flush()
}
