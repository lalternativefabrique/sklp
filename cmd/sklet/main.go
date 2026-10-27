// sklet is the node-side agent of the deploy split: it IS a node, where sklp
// talks to one. It supervises the spaces deployed on this machine — applying a
// local manifest, running the reconcile loop under systemd --user (or a
// daemonless setsid detach), and serving the control API that `sklp deploy`
// pilots from a workstation.
//
// It deliberately ships WITHOUT the dev/CI/publish surface: a production node
// has no reason to carry a pipeline engine.
//
// Verbs:
//
//	sklet apply <stack>  — apply .sklp/<stack>/deploy.yaml on THIS node
//	sklet __supervise    — internal daemon, started by systemd or the setsid detach
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/sklet"
)

const usage = `sklet — node-side deploy agent

Usage:
  sklet <verb> [flags...]

Verbs:
  apply       Apply a stack from .sklp/<stack>/deploy.yaml on THIS node
              ('sklet apply' with no stack lists the choices; '--space' selects
              the environment within the manifest). Supervises detached under
              systemd --user when available, else a setsid daemon.
              '--listen :9443' additionally exposes the control API over TLS +
              a Bearer token so a workstation can pilot this node with
              'sklp deploy --server https://host:9443'.
  reconcile   Pull the repos declared in reconcile.yaml and apply what changed
              (the node reaches Git outbound; nothing reaches the node)
  host        Prepare this Linux host to run rootless spaces ('sklet host setup';
              run it once via sudo before the first apply)
  upgrade     Install a published sklet and restart the agent, rolling back
              if it does not stay up (no sudo once the launcher is installed)
  version     Print the sklet binary version

Examples:
  sklet reconcile                      # poll and apply what Git declares
  sklet reconcile --once               # one pass, then exit
  sklet apply production
  sklet apply core --space production --listen :9443
  sklet apply core --foreground        # debug: supervise in this terminal
  sudo sklet host setup                # one-time: uidmap, subuid, lingering, launcher
  sklet upgrade                        # latest published sklet, health-checked

Piloting from a workstation uses sklp, not sklet:
  sklp deploy login --server https://host:9443 --token <T> --ca-fingerprint <FP>
  sklp deploy ls
  sklp deploy logs <svc> -f

Run 'sklet <verb> -h' for verb-specific flags.
`

func main() {
	if sklet.IsLauncher() {
		os.Exit(sklet.RunLauncher(os.Args[1:]))
	}
	// Fast path for the space network worker: when sklet was re-exec'd by
	// spacenet into a fresh user+net namespace to own a space bridge, this
	// becomes that worker (or a netns holder) and never returns. Also handles
	// the paired OCI hook re-execs. MUST run before any other work in main().
	maybeRunSpaceNetWorker()

	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	root := rootCommand()

	switch os.Args[1] {
	case "-h", "--help", "help":
		fmt.Fprint(os.Stdout, usage)
		return
	case "-v", "--version":
		runVersion(os.Args[2:])
		return
	}

	if tok := os.Args[1]; len(tok) > 0 && tok[0] != '-' && findRootSub(root, tok) == nil {
		fmt.Fprintf(os.Stderr, "sklet: unknown verb %q\n\n%s", tok, usage)
		os.Exit(2)
	}

	adapter := cli.NewFlagAdapter()
	os.Exit(adapter.Execute(context.Background(), root, os.Args[1:]))
}

// findRootSub reports the root command matching tok (by Name or Alias), or nil.
func findRootSub(root cli.Command, tok string) *cli.Command {
	for i := range root.Sub {
		c := &root.Sub[i]
		if c.Name == tok {
			return c
		}
		for _, al := range c.Aliases {
			if al == tok {
				return c
			}
		}
	}
	return nil
}
