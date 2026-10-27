package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/lalternativefabrique/sklp/internal/cli"
)

var jsonFlag = cli.Flag{Name: "json", Type: cli.BoolFlag, Usage: "emit JSON on stdout (the form for scripts and AI agents)"}

func writeJSON(w io.Writer, verb string, v any) int {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", verb, err)
		return 1
	}
	return 0
}
