package devstack

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lalternativefabrique/sklp/internal/layout"
)

// Main is the `sklp dev <stack>` entrypoint: a thin flag-parsing shim that
// resolves .sklp/<stack>/dev.yaml via the layout package, builds a DevConfig
// and delegates to Up. Returns an exit code rather than calling os.Exit so the
// caller (cmd/sklp) controls process lifecycle.
//
// With no positional <stack> (and not a sub-verb) it lists the dev-capable
// stacks and exits 0. The dev.yaml path is no longer a flag: everything lives
// under .sklp/<stack>/.
func Main(args []string) int {
	// `sklp dev down` is a sub-verb, not a flag: it reconstructs the
	// teardown from the on-disk statefile in a fresh process. Routed
	// here (before flag parsing) so `down`'s own flags don't collide
	// with the launch flags below — and before the positional <stack>
	// so a stack can never shadow a sub-verb.
	if len(args) > 0 && args[0] == "down" {
		relayDownLima(args[1:])
		return RunDown(args[1:])
	}
	// `sklp dev clean` moved to the single housekeeping verb. Kept as a routed
	// sub-verb rather than falling through to the launcher, which would read
	// "clean" as a stack name and fail with a confusing "no such stack".
	if len(args) > 0 && args[0] == "clean" {
		fmt.Fprintln(os.Stderr, "sklp dev clean has moved: use `sklp clean` (add --deep to also drop idle spaces and their volumes)")
		return 2
	}

	fs := flag.NewFlagSet("sklp dev", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	repo := fs.String("repo", ".", "repo root (cwd of services with no workdir)")
	noColor := fs.Bool("no-color", false, "disable colored log prefixes")
	graceMs := fs.Int("grace-ms", 5000, "milliseconds to wait between SIGTERM and SIGKILL on shutdown")
	validate := fs.Bool("validate", false, "parse and validate the devstack YAML without launching anything")
	check := fs.Bool("check", false, "with --validate: report which auto-fixes would apply, but do not write the file")
	only := fs.String("only", "", "comma-separated subset of services to launch (plus their transitive deps)")

	// Parse flags then a trailing positional, interleaved: Go's flag stops at
	// the first non-flag, so we parse, take the leading residual as <stack>,
	// then parse what follows. This lets the user put <stack> in any position
	// (`sklp dev stack -validate` or `sklp dev -validate stack`) without a
	// flag after the stack being silently dropped.
	stack, err := parseWithStack(fs, args)
	if err != nil {
		return 2
	}

	repoAbs, err := filepath.Abs(*repo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sklp dev: resolve -repo: %v\n", err)
		return 2
	}

	lay := layout.New(repoAbs)

	// No positional <stack>: catalogue mode. List the dev-capable stacks
	// (deterministic) and exit 0.
	if stack == "" {
		stacks, err := lay.StacksFor("dev")
		if err != nil {
			fmt.Fprintf(os.Stderr, "sklp dev: %v\n", err)
			return 2
		}
		if len(stacks) == 0 {
			fmt.Fprintln(os.Stdout, "sklp dev: no dev stacks under .sklp/ (each stack needs a dev.yaml)")
			return 0
		}
		fmt.Fprintln(os.Stdout, "Available dev stacks (run with `sklp dev <stack>`):")
		for _, s := range stacks {
			fmt.Fprintf(os.Stdout, "  %s\n", s)
		}
		return 0
	}

	taskPath, err := lay.Resolve(stack, "dev")
	if err != nil {
		switch {
		case errors.Is(err, layout.ErrStackNotFound):
			fmt.Fprintf(os.Stderr, "sklp dev: unknown stack %q\n", stack)
			if stacks, _ := lay.StacksFor("dev"); len(stacks) > 0 {
				fmt.Fprintf(os.Stderr, "  available: %s\n", strings.Join(stacks, ", "))
			}
		default: // ErrNoContract: the message already lists the stack's verbs.
			fmt.Fprintf(os.Stderr, "sklp dev: %v\n", err)
		}
		return 2
	}

	var onlyNames []string
	if *only != "" {
		for _, s := range strings.Split(*only, ",") {
			if s = strings.TrimSpace(s); s != "" {
				onlyNames = append(onlyNames, s)
			}
		}
	}

	cfg := DevConfig{
		Stack:    stack,
		TaskPath: taskPath,
		Repo:     repoAbs,
		Validate: *validate,
		Check:    *check,
		NoColor:  *noColor,
		GraceMs:  *graceMs,
		Only:     onlyNames,
		Stdout:   os.Stdout,
		Stderr:   os.Stderr,
	}
	if err := Up(context.Background(), cfg); err != nil {
		if msg := err.Error(); msg != "" {
			fmt.Fprintf(os.Stderr, "%s\n", msg)
		}
		return devExitCode(err)
	}
	return 0
}

// parseWithStack parses fs while tolerating a single positional <stack> token
// anywhere among the flags. flag.Parse stops at the first non-flag, so we parse
// in segments: parse, capture the first residual non-flag as the stack, parse
// the rest, repeat. This correctly skips flag VALUES (`-repo <dir>` does not
// make <dir> the stack) because fs already consumed them. Empty stack = none.
func parseWithStack(fs *flag.FlagSet, args []string) (stack string, err error) {
	for {
		if err = fs.Parse(args); err != nil {
			return "", err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return stack, nil
		}
		if stack == "" {
			stack = rest[0] // first bare token is the stack
		}
		args = rest[1:] // continue parsing flags that followed the positional
	}
}
