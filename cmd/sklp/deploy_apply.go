package main

import (
	"fmt"
	"os"
)

// deploySubverbs are the tokens after `sklp deploy` that name a sub-verb; any
// other leading non-flag token is a stack to apply. Read off the command tree,
// so a new sub-verb is never mistaken for a stack; the two hidden ones the
// tree does not list are added by hand.
var deploySubverbs = func() map[string]bool {
	m := map[string]bool{"agent": true, "__supervise": true}
	for _, c := range deployCommand().Sub {
		m[c.Name] = true
		for _, a := range c.Aliases {
			m[a] = true
		}
	}
	return m
}()

// normalizeDeployApply turns `deploy apply <stack> …` into the bare form the
// transport shims and the façade already understand, and points a bare
// `deploy <stack>` at the explicit verb. args is everything after "deploy".
func normalizeDeployApply(args []string) []string {
	lead := -1
	for i, a := range args {
		if a == "" {
			continue
		}
		if a[0] == '-' {
			break
		}
		lead = i
		break
	}
	if lead < 0 {
		return args
	}
	tok := args[lead]
	if tok == "apply" {
		return append(append([]string{}, args[:lead]...), args[lead+1:]...)
	}
	if !deploySubverbs[tok] && tok != "help" {
		fmt.Fprintf(os.Stderr, "sklp deploy: applying stack %q; the explicit form is `sklp deploy apply %s`\n", tok, tok)
	}
	return args
}
