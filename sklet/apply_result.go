package sklet

import (
	"fmt"

	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

// PrintApplyResult renders a control-API ApplyResult. Shared by the node-side
// launch paths and the sklp client's remote apply — both print the same outcome
// for the same reconcile, so the rendering lives in one place.
func PrintApplyResult(space string, res control.ApplyResult) {
	switch res.Result {
	case control.ResultUnchanged:
		fmt.Printf("space %q: unchanged\n", space)
	case control.ResultCreated:
		fmt.Printf("space %q: created (%s)\n", space, PluralChanges(len(res.Changes)))
		for _, c := range res.Changes {
			fmt.Printf("  - %s\n", c)
		}
	case control.ResultRejected:
		fmt.Printf("space %q: rejected (%s)\n", space, PluralChanges(len(res.Changes)))
		for _, c := range res.Changes {
			fmt.Printf("  - %s\n", c)
		}
	default:
		fmt.Printf("space %q: configured (%s)\n", space, PluralChanges(len(res.Changes)))
		for _, c := range res.Changes {
			fmt.Printf("  - %s\n", c)
		}
	}
}

// PluralChanges formats a change count: "1 change" / "N changes".
func PluralChanges(n int) string {
	if n == 1 {
		return "1 change"
	}
	return fmt.Sprintf("%d changes", n)
}
