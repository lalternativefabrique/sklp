package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/internal/xdg"
	"github.com/lalternativefabrique/sklp/sklet"
	"github.com/lalternativefabrique/sklp/space"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

func spaceCommand() cli.Command {
	return cli.Command{
		Name:  "space",
		Short: "Operate one space of this node",
		Sub: []cli.Command{
			{
				Name:  "migrate-owner",
				Short: "Hand a stopped space's volumes to its own root (child user namespace), or back with --back; only the operator's files move",
				Flags: []cli.Flag{
					{Name: "back", Type: cli.BoolFlag, Usage: "hand the volumes back to the operator (return to the shared user namespace)"},
				},
				Run: func(ctx context.Context, args cli.Args) int {
					pos := args.Positionals()
					if len(pos) != 1 || pos[0] == "" {
						fmt.Fprintln(os.Stderr, "usage: sklet space migrate-owner [--back] <space>")
						return 2
					}
					name := pos[0]
					if reason := spaceMayBeRunning(name); reason != "" {
						fmt.Fprintf(os.Stderr, "sklet space migrate-owner: %s; stop it first (a running service would keep writing as the old owner)\n", reason)
						return 1
					}
					if err := inheritNodeDataRoot(); err != nil {
						fmt.Fprintln(os.Stderr, "sklet space migrate-owner:", err)
						return 1
					}
					sp, err := space.NewSpace(name)
					if err != nil {
						fmt.Fprintln(os.Stderr, "sklet space migrate-owner:", err)
						return 1
					}
					moved, err := sp.MigrateVolumeOwner(args.Bool("back"))
					for _, v := range moved {
						fmt.Printf("%s: volume %s migrated\n", name, v)
					}
					if err != nil {
						fmt.Fprintln(os.Stderr, "sklet space migrate-owner:", err)
						return 1
					}
					if len(moved) == 0 {
						fmt.Printf("%s: no named volume\n", name)
					}
					return 0
				},
			},
		},
	}
}

// inheritNodeDataRoot resolves the data disk the agent keeps volumes on, the
// way the agent learns it: the environment, then node.yaml (ADR-020), then
// the environment of the agent's unit for a node configured before it.
func inheritNodeDataRoot() error {
	if os.Getenv(xdg.DataRootEnv) != "" {
		return nil
	}
	if cfg, err := sklet.LoadNodeConfig(sklet.NodeConfigPath()); err == nil && cfg.DataRoot != "" {
		return os.Setenv(xdg.DataRootEnv, cfg.DataRoot)
	}
	out, err := exec.Command("systemctl", "--user", "show", "-p", "Environment", "--value", "sklet-agent").Output()
	if err != nil {
		return nil
	}
	for _, kv := range strings.Fields(string(out)) {
		if v, ok := strings.CutPrefix(kv, xdg.DataRootEnv+"="); ok {
			return os.Setenv(xdg.DataRootEnv, v)
		}
	}
	return nil
}

// spaceMayBeRunning names what still runs the space, or "" when nothing does:
// the node agent (it supervises every space of the node) or a standalone
// `sklet apply` supervisor.
func spaceMayBeRunning(name string) string {
	if out, err := exec.Command("systemctl", "--user", "is-active", "sklet-agent").Output(); err == nil && strings.TrimSpace(string(out)) == "active" {
		return "sklet-agent is active"
	}
	if pid, err := control.ReadSupervisorPid(name); err == nil {
		if control.ProcessAlive(pid) {
			return fmt.Sprintf("its supervisor (pid %d) is alive", pid)
		}
	}
	return ""
}
