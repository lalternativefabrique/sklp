package main

import (
	"context"
	"fmt"
	"os"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/pipeline/runnerimage"
)

// runnerCommand declares `sklp runner build` — build the project runner image
// from .sklp/space.yaml.
func runnerCommand() cli.Command {
	return cli.Command{
		Name:  "runner",
		Short: "Build the project runner image from .sklp/space.yaml",
		Sub: []cli.Command{
			{
				Name:  "build",
				Short: "Build (or print) the runner image",
				Flags: []cli.Flag{
					{Name: "repo", Type: cli.StringFlag, StringDefault: ".", Usage: "path of the project repo containing .sklp/space.yaml"},
					{Name: "config", Type: cli.StringFlag, Usage: "explicit path to the space recipe (default: <repo>/.sklp/space.yaml)"},
					{Name: "tag", Type: cli.StringFlag, StringDefault: "sklp-runner:dev", Usage: "tag for the produced image"},
					{Name: "print", Type: cli.BoolFlag, Usage: "print the generated Dockerfile and exit without building"},
				},
				Run: func(ctx context.Context, args cli.Args) int {
					return runRunnerBuild(args.String("repo"), args.String("config"), args.String("tag"), args.Bool("print"))
				},
			},
		},
	}
}

// runRunnerBuild builds (or, with printOnly, renders) the runner image, returning
// the process exit code.
func runRunnerBuild(repo, configPath, tag string, printOnly bool) int {
	if printOnly {
		cp := configPath
		if cp == "" {
			cp = runnerimage.ResolvePath(repo)
		}
		cfg, err := runnerimage.Load(cp)
		if err != nil {
			fmt.Fprintln(os.Stderr, "sklp runner build:", err)
			return 1
		}
		df, err := runnerimage.RenderDockerfile(cfg.Runner)
		if err != nil {
			fmt.Fprintln(os.Stderr, "sklp runner build:", err)
			return 1
		}
		fmt.Print(df)
		return 0
	}
	if err := runnerimage.Build(runnerimage.BuildOptions{
		RepoRoot:   repo,
		ConfigPath: configPath,
		Tag:        tag,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "sklp runner build:", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "[sklp runner] built %s\n", tag)
	return 0
}
