package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"

	"github.com/lalternativefabrique/sklp/internal/cli"
)

// version is the human-readable release tag. The release pipeline
// overrides it via:
//
//	go build -ldflags "-X main.version=v0.5.0 -X main.commit=abcdef0"
//
// In dev builds (plain `go install ./cmd/sklp`) the linker leaves
// these defaults in place and we fall back to runtime/debug's vcs
// info so `sklp version` still says something useful.
var (
	version = "dev"
	commit  = ""
	date    = ""
)

// versionCommand declares `sklp version` on the façade. -short (alias -s) prints
// only the bare version tag for shell scripting.
func versionCommand() cli.Command {
	return cli.Command{
		Name:    "version",
		Aliases: []string{"-v", "--version"},
		Short:   "Print the sklp binary version (use -short for the bare tag)",
		Flags: []cli.Flag{
			{Name: "short", Aliases: []string{"s"}, Type: cli.BoolFlag, Usage: "print only the version string"},
		},
		Run: func(ctx context.Context, args cli.Args) int {
			printVersion(args.Bool("short"))
			return 0
		},
	}
}

// runVersion is the root-level `-v`/`--version` path (main dispatches it directly,
// ahead of the façade). It honours the bare `-short`/`-s` token for parity.
//
// Default output (one line per field, easy to grep):
//
//	sklp v0.5.0 (commit abc1234, built 2026-05-14, go1.25.0 linux/amd64)
func runVersion(args []string) {
	short := false
	for _, a := range args {
		if a == "-short" || a == "--short" || a == "-s" {
			short = true
		}
		if a == "-h" || a == "--help" {
			fmt.Fprintln(os.Stderr, "Usage: sklp version [-short]")
			os.Exit(0)
		}
	}
	printVersion(short)
}

// printVersion renders the version line (or the bare tag when short).
func printVersion(short bool) {
	v, c, d := resolveVersion()
	if short {
		fmt.Println(v)
		return
	}
	fmt.Printf("sklp %s (commit %s, built %s, %s %s/%s)\n",
		v, ellipsis(c, 7), d, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// resolveVersion combines the ldflags-injected values with whatever
// runtime/debug knows about the build (when ldflags is absent). The
// fallbacks make a dev binary self-describing even without a release
// pipeline.
func resolveVersion() (v, c, d string) {
	v, c, d = version, commit, date

	// debug.ReadBuildInfo returns vcs.revision and vcs.time when the
	// binary was built from a clean git checkout via `go install` or
	// `go build`. We only override empty fields so explicit ldflags
	// values always win.
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if c == "" {
				c = s.Value
			}
		case "vcs.time":
			if d == "" {
				d = s.Value
			}
		case "vcs.modified":
			if s.Value == "true" && v == "dev" {
				v = "dev-dirty"
			}
		}
	}
	if c == "" {
		c = "unknown"
	}
	if d == "" {
		d = "unknown"
	}
	return
}

// ellipsis trims a string to at most n runes. Used so the full git
// SHA isn't dumped into the one-line output; 7 chars is the human
// convention.
func ellipsis(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
