package main

import (
	"context"
	"fmt"
	"os"

	"github.com/lalternativefabrique/sklp/devstack"
	"github.com/lalternativefabrique/sklp/internal/cli"
)

// rootCommand assembles the full sklp verb tree as declarative cli.Command
// values. Each verb's Run is thin transport glue: it reads the verbatim tail
// (RawArgs) and hands it to the SAME underlying helper the legacy switch called,
// so migrating to the façade moves only the dispatch, never the business logic.
//
// Verbs whose internal parsing is hand-rolled (positional interleaving, docker-
// style exec, `--` splits) stay RawArgs and keep owning their flag parsing; the
// façade gives them a single uniform dispatch + help surface. The new space/
// deploy sub-trees (phase 3) declare real façade flags over the Registry.
func rootCommand() cli.Command {
	return cli.Command{
		Name:  "sklp",
		Short: "dev → CI → publish runner",
		Long:  usage,
		Sub: []cli.Command{
			initCommand(),
			skillsCommand(),
			runCommand(),
			{
				Name:    "dev",
				Short:   "Run a stack's local dev environment (.sklp/<stack>/dev.yaml)",
				RawArgs: true,
				Run: func(ctx context.Context, args cli.Args) int {
					return devstack.Main(args.Passthrough())
				},
			},
			flowCommand(),
			cockpitCommand(),
			spaceCommand(),
			agentCommand(),
			imageCommand(),
			deployCommand(),
			gatewayCommand(),
			authCommand(),
			registryCommand(),
			issueCommand(),
			projectCommand(),
			whoamiCommand(),
			agentsMDCommand(),
			secretCommand(),
			flagCommand(),
			ciCommand(),
			logsCommand(),
			cacheCommand(),
			macosCommand(),
			macosDaemonCommand(),
			cleanCommand(),
			configCommand(),
			runnerCommand(),
			hostCommand(),
			pauseCommand(),
			rawCmd("completion", "Emit shell completion script (bash | zsh | fish)", runCompletion),
			versionCommand(),
			{
				// Hidden: shells call this for dynamic stack/pipeline lists.
				Name:    "__complete",
				Hidden:  true,
				RawArgs: true,
				Run: func(ctx context.Context, args cli.Args) int {
					runComplete(args.Passthrough())
					return 0
				},
			},
			{
				// Hidden: emits the command tree as JSON, the single source of
				// truth for the sklp-web documentation site (the sklp-web docs site).
				Name:   "__doc",
				Hidden: true,
				Run: func(ctx context.Context, args cli.Args) int {
					if err := cli.WriteDoc(os.Stdout, rootCommand()); err != nil {
						fmt.Fprintln(os.Stderr, "sklp __doc:", err)
						return 1
					}
					return 0
				},
			},
		},
	}
}

// runCommand declares `sklp run`. It is RawArgs because the bulk of its tail is
// the ~29 runner.Main flags (-base-ref, -output, -runner-image, …) which live in
// package runner — the single source shared with the core/k8s spawner — and must
// NOT be re-declared here (that would duplicate the flagset and drift). What we
// DO declare is run's own CLI surface — the <name> positional and -file — purely
// so help + completion know it (the positional completes pipeline names via the
// `run` __complete subject). The verbatim tail still flows to runRunVerb →
// runner.Main; the façade does not validate it (intentional escape hatch).
func runCommand() cli.Command {
	return cli.Command{
		Name:    "run",
		Short:   "Run a pipeline from .sklp/pipelines/<name>.yaml (or -file <path>)",
		RawArgs: true,
		Positionals: []cli.Positional{
			{Name: "name", Complete: "run"}, // optional: bare `run` lists pipelines
		},
		Flags: []cli.Flag{
			{Name: "file", Type: cli.StringFlag, Usage: "path to a pipeline YAML outside .sklp/pipelines/"},
		},
		Run: func(ctx context.Context, args cli.Args) int {
			runRunVerb(args.Passthrough())
			return 0
		},
	}
}

// rawCmd builds a RawArgs verb whose Run hands the verbatim tail to fn (which
// owns its own parsing + os.Exit, unchanged from the legacy switch).
func rawCmd(name, short string, fn func([]string)) cli.Command {
	return cli.Command{
		Name:    name,
		Short:   short,
		RawArgs: true,
		Run: func(ctx context.Context, args cli.Args) int {
			fn(args.Passthrough())
			return 0
		},
	}
}
