package main

import (
	"fmt"
	"os"
)

// deploySubverbs are the tokens after `sklp deploy` that name a sub-verb; any
// other leading non-flag token is a stack to apply.
var deploySubverbs = map[string]bool{
	"apply": true, "ls": true, "get": true, "logs": true, "exec": true, "restart": true,
	"delete": true, "login": true, "context": true, "token": true, "fingerprint": true,
	"mesh": true, "datastores": true, "restores": true, "agent": true, "__supervise": true,
}

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
