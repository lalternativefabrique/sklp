// flow.go implements `sklp flow {start,end}` — the feature lifecycle verb.
//
//	flow start <name>  opens branch feat/<name> in a dedicated worktree (or
//	                   in place with --branch), starting from --onto (main).
//	flow end           diffs the current branch against --onto (main) and
//	                   runs the CI pipeline scoped to the impacted services.
//
// The Run bodies are thin transport glue: git mechanics live in internal/flow,
// and `flow end` reuses the existing runner (runRunVerb) instead of
// reimplementing the CI — it just feeds the target ref as -base-ref.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/lalternativefabrique/sklp/cmd/sklp/internal/flow"
	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/internal/layout"
	"github.com/lalternativefabrique/sklp/pipeline"
	"github.com/lalternativefabrique/sklp/pipeline/pipelineselect"
)

func flowCommand() cli.Command {
	ontoFlag := cli.Flag{Name: "onto", Type: cli.StringFlag, StringDefault: "main", Usage: "target ref the feature builds on / diffs against"}
	return cli.Command{
		Name:  "flow",
		Short: "Feature lifecycle: open a branch/worktree (start), diff vs target + run CI (end)",
		Sub: []cli.Command{
			{
				Name:        "start",
				Short:       "Open feat/<name> in a worktree (default, --worktree) or in place (--branch) from --onto",
				Positionals: []cli.Positional{{Name: "name", Required: true}},
				Flags: []cli.Flag{
					ontoFlag,
					{Name: "worktree", Type: cli.BoolFlag, Usage: "create a dedicated worktree (default; explicit opt-in)"},
					{Name: "branch", Type: cli.BoolFlag, Usage: "checkout in place instead of creating a dedicated worktree"},
				},
				Run: func(ctx context.Context, args cli.Args) int {
					return runFlowStart(args)
				},
			},
			{
				Name:  "end",
				Short: "Push the branch and open a PR into --onto; CI runs on the PR",
				Long: "Diff the current branch vs --onto, push it and open a PR. CI then runs\n" +
					"on the PR itself, so the pipeline is not executed here: running it\n" +
					"locally too would do the same work twice on the same commit.\n\n" +
					"--local runs the pipeline on this machine instead and pushes nothing,\n" +
					"for checking a change before it leaves the laptop.",
				Flags: []cli.Flag{
					ontoFlag,
					{Name: "pipeline", Type: cli.StringFlag, Usage: "pipeline name under .sklp/pipelines/ to run with --local (empty = auto-select by scope match)"},
					{Name: "local", Aliases: []string{"test"}, Type: cli.BoolFlag, Usage: "run the pipeline on this machine instead of opening a PR"},
					{Name: "no-test", Type: cli.BoolFlag, Usage: "only show the diff + impacted scope; do nothing else"},
				},
				Run: func(ctx context.Context, args cli.Args) int {
					return runFlowEnd(args)
				},
			},
		},
	}
}

func runFlowStart(args cli.Args) int {
	pos := args.Positionals()
	if len(pos) == 0 {
		fmt.Fprintln(os.Stderr, "sklp flow start: a feature name is required")
		return 2
	}
	if args.Bool("worktree") && args.Bool("branch") {
		fmt.Fprintln(os.Stderr, "sklp flow start: --worktree and --branch are mutually exclusive")
		return 2
	}
	res, err := flow.Start(flow.StartOptions{
		Name:       pos[0],
		Onto:       args.String("onto"),
		BranchOnly: args.Bool("branch"),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp flow start:", err)
		return 1
	}
	if res.Worktree != "" {
		fmt.Printf("created branch %s in worktree %s\n", res.Branch, res.Worktree)
		fmt.Printf("  cd %s\n", res.Worktree)
	} else {
		fmt.Printf("created and checked out branch %s\n", res.Branch)
	}
	return 0
}

func runFlowEnd(args cli.Args) int {
	res, err := flow.End(flow.EndOptions{Onto: args.String("onto")})
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp flow end:", err)
		return 1
	}

	fmt.Printf("branch %s vs %s — %d file(s) changed\n", res.Branch, res.Onto, len(res.Changed))
	for _, f := range res.Changed {
		fmt.Printf("  %s\n", f)
	}
	if len(res.Changed) == 0 {
		fmt.Fprintf(os.Stderr, "nothing changed vs %s; skipping tests\n", res.Onto)
		return 0
	}
	// Default path: push and open the PR. CI runs on the PR, so the pipeline
	// is deliberately NOT executed here — doing both would run the same
	// pipeline twice on the same commit, and the node runs one at a time.
	if !args.Bool("local") && !args.Bool("no-test") {
		return pushAndOpenPR(res)
	}

	pipelinesDir := layout.New(".").PipelinesDir()

	// Resolve which pipelines to run: explicit override, or auto-select by scope.
	var selected []pipelineselect.SelectedPipeline
	if override := args.String("pipeline"); override != "" {
		taskFile := filepath.Join(pipelinesDir, override+".yaml")
		if _, err := os.Stat(taskFile); err != nil {
			fmt.Fprintf(os.Stderr, "sklp flow end: pipeline %q not found at %s\n", override, taskFile)
			return 2
		}
		selected = []pipelineselect.SelectedPipeline{{Name: override, Path: taskFile}}
	} else {
		selected, err = pipelineselect.SelectPipelines(pipelinesDir, res.Branch, res.Changed)
		if err != nil {
			fmt.Fprintln(os.Stderr, "sklp flow end:", err)
			return 1
		}
		if len(selected) == 0 {
			fmt.Println("no pipeline scope matches the diff; nothing to run")
			for _, f := range res.Changed {
				fmt.Printf("  %s\n", f)
			}
			return 0
		}
	}

	names := make([]string, len(selected))
	for i, p := range selected {
		names[i] = p.Name
	}
	fmt.Printf("%d pipeline(s) selected: %s\n", len(selected), strings.Join(names, ", "))
	if args.Bool("no-test") {
		return 0
	}

	// Run ALL selected pipelines (no fail-fast) and aggregate the results so the
	// summary is honest about every service, then gate push/PR on full green.
	// A require: violation (e.g. a main-only pipeline selected on a feat branch)
	// is a SKIP, not a failure — it never blocks push/PR.
	var results []pipelineResult
	firstFail := 0
	// Same reason as `sklp run`: a pipeline spawns detached services whose only
	// reaper is the deferred teardown, so an unhandled SIGINT/SIGTERM would
	// strand them. Cancel instead of dying.
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	for _, p := range selected {
		fmt.Printf("running pipeline %q (base-ref %s)\n", p.Name, res.Onto)
		_, runErr := pipeline.ParseArgsAndRun(ctx, []string{
			"-task", p.Path, "-repo", ".", "-base-ref", res.Onto,
		})
		r := classifyRunErr(p.Name, runErr)
		if r.state == pipelineFailed {
			fmt.Fprintln(os.Stderr, "sklp flow end:", runErr)
			if firstFail == 0 {
				firstFail = r.code
			}
		}
		results = append(results, r)
	}

	fmt.Println("flow end summary:")
	for _, r := range results {
		switch r.state {
		case pipelinePassed:
			fmt.Printf("  %-12s ✓\n", r.name)
		case pipelineSkipped:
			fmt.Printf("  %-12s ⏭ skipped (require)\n", r.name)
		default:
			fmt.Printf("  %-12s ✗ (exit %d)\n", r.name, r.code)
		}
	}
	if firstFail != 0 {
		// CI red: stop here, no push, no PR. The dev fixes and re-runs.
		return firstFail
	}

	// --local stops here: it exists to check a change before it leaves the
	// machine, so it never pushes.
	return 0
}

// pushAndOpenPR publishes the branch and opens (or updates) its PR. CI is left
// to the PR: the webhook dispatches it to the CI node, and the run is followed
// in the UI rather than in this terminal.
func pushAndOpenPR(res flow.EndResult) int {
	fmt.Printf("pushing %s and syncing PR into %s\n", res.Branch, res.Onto)
	if err := flow.Push(res.Branch); err != nil {
		fmt.Fprintln(os.Stderr, "sklp flow end:", err)
		return 1
	}
	url, created, err := flow.OpenPR(res.Branch, res.Onto, "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp flow end:", err)
		return 1
	}
	if created {
		fmt.Printf("opened PR: %s\n", url)
	} else {
		fmt.Printf("updated existing PR: %s\n", url)
	}
	fmt.Println("CI runs on the PR — follow it in the Skalpai UI")
	return 0
}

// pipelineState is the aggregated outcome of one selected pipeline.
type pipelineState int

const (
	pipelinePassed pipelineState = iota
	pipelineFailed
	pipelineSkipped // require: violation — not allowed to run here, not a failure.
)

type pipelineResult struct {
	name  string
	state pipelineState
	code  int
}

// classifyRunErr maps a ParseArgsAndRun error to an aggregated result.
// A require: violation is a SKIP (must not fail flow end); any other
// non-nil error is a failure with a non-zero exit code.
func classifyRunErr(name string, runErr error) pipelineResult {
	if runErr == nil {
		return pipelineResult{name: name, state: pipelinePassed}
	}
	if errors.Is(runErr, pipeline.ErrRequireViolation) {
		return pipelineResult{name: name, state: pipelineSkipped}
	}
	code := pipeline.ExitCode(runErr)
	if code == 0 {
		code = 1 // a non-nil error always counts as a failure
	}
	return pipelineResult{name: name, state: pipelineFailed, code: code}
}
