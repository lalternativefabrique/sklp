// deploy_context.go implements `sklp deploy context {list,use,current}` — the
// kubeconfig-style context switcher for the deploy control transport. A context
// is server + token + ca-fingerprint + space; the ACTIVE one (config
// `current_context`) decides which remote supervisor a plain `sklp deploy`
// pilots. `--context <name>` overrides per command; `--local` is the only way
// to target the local socket — an unresolved context is an error, never a
// silent fall back to the developer's own machine.
// Non-gated: it only reads/writes config, no embedded runtime needed.
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/internal/config"
)

// deployContextCommand declares `sklp deploy context <list|use|current>`.
func deployContextCommand() cli.Command {
	return cli.Command{
		Name:  "context",
		Short: "Select the active deploy target (kubeconfig-style): list | use | current",
		// Bare `sklp deploy context` lists, mirroring `kubectl config get-contexts`.
		Run: func(ctx context.Context, args cli.Args) int { return runContextList() },
		Sub: []cli.Command{
			{
				Name:  "list",
				Short: "List configured contexts; marks the active one with *",
				Run:   func(ctx context.Context, args cli.Args) int { return runContextList() },
			},
			{
				Name:        "use",
				Short:       "Set the active context (like `kubectl config use-context`)",
				Positionals: []cli.Positional{{Name: "name"}},
				Run: func(ctx context.Context, args cli.Args) int {
					return runContextUse(firstArg(args, 0))
				},
			},
			{
				Name:  "current",
				Short: "Print the active context and where it points",
				Run:   func(ctx context.Context, args cli.Args) int { return runContextCurrent() },
			},
		},
	}
}

// runContextList prints one row per configured context: the active one carries a
// leading "*", the target column shows the remote server.
func runContextList() int {
	cfg, _, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy context:", err)
		return 1
	}
	if len(cfg.Contexts) == 0 {
		// `sklp auth login` is deliberately NOT suggested here: it configures the
		// SaaS endpoint, which creates no deploy context.
		fmt.Println("no contexts configured — create one with `sklp deploy login`")
		return 0
	}
	names := make([]string, 0, len(cfg.Contexts))
	for n := range cfg.Contexts {
		names = append(names, n)
	}
	sort.Strings(names)

	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "CURRENT\tNAME\tTARGET\tSPACE")
	for _, n := range names {
		c := cfg.Contexts[n]
		mark := ""
		if n == cfg.CurrentContext {
			mark = "*"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", mark, n, contextTarget(c), contextSpace(c))
	}
	w.Flush()
	return 0
}

// runContextUse sets cfg.CurrentContext to name (must already exist) and
// persists it.
func runContextUse(name string) int {
	if name == "" {
		fmt.Fprintln(os.Stderr, "sklp deploy context use: a context name is required")
		return 2
	}
	cfg, path, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy context use:", err)
		return 1
	}
	if _, ok := cfg.Contexts[name]; !ok {
		fmt.Fprintf(os.Stderr, "sklp deploy context use: context %q not found (see `sklp deploy context list`)\n", name)
		return 1
	}
	cfg.CurrentContext = name
	if err := cfg.Save(path); err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy context use:", err)
		return 1
	}
	c := cfg.Contexts[name]
	fmt.Printf("switched to context %q → %s (space %s)\n", name, contextTarget(c), contextSpace(c))
	return 0
}

// runContextCurrent prints the active context name and where a plain `sklp
// deploy` would go with it.
func runContextCurrent() int {
	cfg, _, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy context current:", err)
		return 1
	}
	name, c, err := cfg.ResolveContext("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy context current:", err)
		return 1
	}
	fmt.Printf("%s\t%s\t%s\n", name, contextTarget(c), contextSpace(c))
	return 0
}

// contextTarget renders where a context points. A context always carries a
// Server in the new shape (`sklp deploy login` requires --server, and the
// legacy migration drops server-less profiles), but a hand-edited config can
// still omit it — say so rather than implying the local socket, which only
// --local reaches.
func contextTarget(c config.Context) string {
	if c.Server != "" {
		return c.Server
	}
	return "(no server — set one with `sklp deploy login --context <name> --server ...`)"
}

// contextSpace renders a context's default space, or a dash when unset (the
// deploy verbs then fall back to their own "production" default).
func contextSpace(c config.Context) string {
	if c.Space != "" {
		return c.Space
	}
	return "-"
}
