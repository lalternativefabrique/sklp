//go:build !linux

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/internal/limarelay"
)

// deployCommand and cockpitCommand keep `sklp deploy` / `sklp cockpit` in the
// verb tree outside Linux so help and completion match across platforms. Both
// drive a node's runc state — the local one through the space runtime, the
// remote one through a control client that speaks the same node-side types —
// so neither has an implementation here. A workstation that runs its builds in
// a Linux VM relays the invocation there before dispatch reaches these stubs.

func deployCommand() cli.Command {
	return cli.Command{
		Name:    "deploy",
		Short:   "Deploy a stack from .sklp/<stack>/deploy.yaml on a node",
		RawArgs: true,
		Run:     func(_ context.Context, _ cli.Args) int { return relayOrNeedsLinux("deploy") },
	}
}

func cockpitCommand() cli.Command {
	return cli.Command{
		Name:    "cockpit",
		Short:   "Interactive dashboard over the running spaces on this node",
		RawArgs: true,
		Run:     func(_ context.Context, _ cli.Args) int { return relayOrNeedsLinux("cockpit") },
	}
}

// relayOrNeedsLinux relays the verb into the relay VM (see limarelay). An
// exec-based provider never returns on success; an in-process one runs the
// guest to completion and reports its exit status, which is this process's
// status too. Only a genuine relay failure falls through to the hint.
func relayOrNeedsLinux(verb string) int {
	cwd, err := os.Getwd()
	if err != nil {
		return needsLinuxNode(verb)
	}
	rerr := limarelay.RelaySelf(cwd)
	if rerr == nil {
		return 0
	}
	var exit *limarelay.GuestExitError
	if errors.As(rerr, &exit) {
		return exit.Code
	}
	fmt.Fprintf(os.Stderr, "sklp %s: relay: %v\n", verb, rerr)
	return needsLinuxNode(verb)
}

func needsLinuxNode(verb string) int {
	fmt.Fprintf(os.Stderr, "sklp %s: needs a Linux node (running on %s)\n", verb, runtime.GOOS)
	return 1
}

// The deploy transport shims run ahead of dispatch in main; off Linux there is
// no deploy tree behind them, so they never claim the invocation.

func maybeRunRemote(args []string) (bool, int) { return false, 0 }

func maybeRunRemoteNet(args []string) (bool, int) { return false, 0 }

func maybeRunRemoteDeploy(args []string) (bool, int) { return false, 0 }
