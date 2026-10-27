// completion.go implements `sklp completion <bash|zsh|fish>` — emits a shell
// completion script to stdout. We follow the kubectl / helm / gh convention:
// users source the output from their shell init file.
//
// The scripts are GENERATED from the cli.Command tree (rootCommand) via the
// package cli generator — the single source of truth — so they can never drift
// from the declared verbs/flags. Dynamic runtime values (stack / pipeline /
// space names) are resolved by shelling back into `sklp __complete <subject>`.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/lalternativefabrique/sklp/internal/cli"
)

const completionUsage = `Usage: sklp completion <bash|zsh|fish>

Emits a shell completion script to stdout.

Install (pick one):
  # Bash
  echo 'source <(sklp completion bash)' >> ~/.bashrc

  # Zsh
  echo 'source <(sklp completion zsh)' >> ~/.zshrc
  # or, with a plugin manager that loads $fpath:
  sklp completion zsh > "${fpath[1]}/_sklp"

  # Fish
  sklp completion fish > ~/.config/fish/completions/sklp.fish
`

// runCompletion is the verb entry point. It writes the requested script to
// stdout, or the usage banner to stderr on bad input. The exit code mirrors the
// rest of the CLI: 0 on success, 2 on usage error.
func runCompletion(args []string) {
	exit := runCompletionTo(os.Stdout, os.Stderr, args)
	if exit != 0 {
		os.Exit(exit)
	}
}

// runCompletionTo is the testable core. Splitting it out lets the test suite
// assert exit codes and captured output without spawning a subprocess.
func runCompletionTo(stdout, stderr io.Writer, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, completionUsage)
		return 2
	}
	root := rootCommand()
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, completionUsage)
		return 0
	case "bash":
		fmt.Fprint(stdout, cli.GenerateBash(root, "sklp"))
		return 0
	case "zsh":
		fmt.Fprint(stdout, cli.GenerateZsh(root, "sklp"))
		return 0
	case "fish":
		fmt.Fprint(stdout, cli.GenerateFish(root, "sklp"))
		return 0
	default:
		fmt.Fprintf(stderr, "sklp completion: unsupported shell %q\n\n%s", args[0], completionUsage)
		return 2
	}
}
