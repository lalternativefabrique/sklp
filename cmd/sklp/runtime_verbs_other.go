//go:build !linux

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/lalternativefabrique/sklp/internal/cli"
)

// space, gateway and clean all act on the space runtime — rootless userns,
// runc state, cgroups, overlay mounts — which has no counterpart outside
// Linux. The verbs stay in the tree so help and completion match across
// platforms; a workstation that runs its builds in a Linux VM relays the
// invocation there before dispatch reaches these stubs.

// spaceSubcommandsNeedingName address one space by name. A missing name is a
// usage error and is reported as one here, before the invocation is relayed:
// otherwise whether the user reads "needs a space name" or "the VM is not
// built" would depend on the state of the VM, which has nothing to do with the
// mistake they made. The check reads the RawArgs tail, so it only fires on a
// bare `space <verb>` — a tail carrying anything else (a name, or flags the
// guest owns like `exec <name> -c web`) relays verbatim.
var spaceSubcommandsNeedingName = map[string]bool{
	"get": true, "down": true, "exec": true, "logs": true,
}

func spaceCommand() cli.Command {
	return cli.Command{
		Name:    "space",
		Short:   "Manage closed environments from .sklp/space.yaml (up | ls | get | down | exec | purge | build)",
		RawArgs: true,
		Run: func(_ context.Context, args cli.Args) int {
			tail := args.Passthrough()
			if len(tail) == 1 && spaceSubcommandsNeedingName[tail[0]] {
				fmt.Fprintf(os.Stderr, "sklp space %s: needs a space name\n", tail[0])
				return 2
			}
			return relayOrNeedsLinux("space")
		},
	}
}

func gatewayCommand() cli.Command {
	return cli.Command{
		Name:    "gateway",
		Short:   "Manage the host-level ingress gateway singleton (up | down | status)",
		RawArgs: true,
		Run:     func(_ context.Context, _ cli.Args) int { return relayOrNeedsLinux("gateway") },
	}
}

func cleanCommand() cli.Command {
	return cli.Command{
		Name:    "clean",
		Short:   "Free disk space: rebuildable caches + dead runtime state (--images, --deep widen it)",
		RawArgs: true,
		Run:     func(_ context.Context, _ cli.Args) int { return relayOrNeedsLinux("clean") },
	}
}
