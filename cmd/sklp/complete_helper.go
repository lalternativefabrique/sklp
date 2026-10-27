// complete_helper.go implements the hidden `sklp __complete <subject>` helper.
// The generated shell completion scripts shell back into it for dynamic
// candidate lists (stacks-for-verb, pipelines, space names) so runtime
// resolution lives once in Go instead of being duplicated across shells.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/lalternativefabrique/sklp/internal/layout"
)

// runComplete is the verb entry point. It prints one candidate per line, sorted,
// and exits 0; an unknown subject prints nothing and exits 0 so completion never
// errors out in the user's shell.
func runComplete(args []string) {
	os.Exit(runCompleteTo(os.Stdout, ".", args))
}

// runCompleteTo is the testable core: writes candidates for the requested
// subject to w, resolving the .sklp/ tree under repo. Returns the exit code.
func runCompleteTo(w io.Writer, repo string, args []string) int {
	if len(args) == 0 {
		return 0
	}
	l := layout.New(repo)
	var names []string
	switch args[0] {
	case "dev", "deploy":
		names, _ = l.StacksFor(args[0])
	case "run":
		names, _ = l.Pipelines()
	case "space":
		// Existing space names from the host runtime registry (cache scan, no
		// runc state). Off-linux this resolves to no candidates — never an error.
		names, _ = listSpaceNames()
	default:
		return 0
	}
	for _, n := range names {
		fmt.Fprintln(w, n)
	}
	return 0
}
