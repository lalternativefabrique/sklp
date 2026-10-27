package main

import "github.com/lalternativefabrique/sklp/internal/cli"

// firstPositional returns the first positional token, or ok=false when none.
func firstPositional(args cli.Args) (string, bool) {
	pos := args.Positionals()
	if len(pos) == 0 {
		return "", false
	}
	return pos[0], true
}
