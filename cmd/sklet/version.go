package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"

	"github.com/lalternativefabrique/sklp/internal/cli"
)

// version is the human-readable release tag. The release pipeline overrides it
// via: go build -ldflags "-X main.version=v0.5.0 -X main.commit=abcdef0". In dev
// builds the linker leaves these defaults and we fall back to runtime/debug's
// vcs info so `sklet version` still says something useful.
var (
	version = "dev"
	commit  = ""
	date    = ""
)

// versionCommand declares `sklet version`. --short (alias -s) prints only the
// bare version tag for shell scripting.
func versionCommand() cli.Command {
	return cli.Command{
		Name:    "version",
		Aliases: []string{"-v", "--version"},
		Short:   "Print the sklet binary version (use --short for the bare tag)",
		Flags: []cli.Flag{
			{Name: "short", Aliases: []string{"s"}, Type: cli.BoolFlag, Usage: "print only the version string"},
		},
		Run: func(ctx context.Context, args cli.Args) int {
			printVersion(args.Bool("short"))
			return 0
		},
	}
}

// runVersion is the root-level -v/--version path (main dispatches it directly,
// ahead of the façade). It honours the bare --short/-s token for parity.
func runVersion(args []string) {
	short := false
	for _, a := range args {
		if a == "-short" || a == "--short" || a == "-s" {
			short = true
		}
		if a == "-h" || a == "--help" {
			fmt.Fprintln(os.Stderr, "Usage: sklet version [--short]")
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
	line := "sklet " + v
	if c != "" {
		line += " (commit " + c
		if d != "" {
			line += ", built " + d
		}
		line += fmt.Sprintf(", %s %s/%s)", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	} else {
		line += fmt.Sprintf(" (%s %s/%s)", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	}
	fmt.Println(line)
}

// resolveVersion returns the version/commit/date, falling back to the build
// info embedded by the Go toolchain when the linker flags were not set.
func resolveVersion() (v, c, d string) {
	v, c, d = version, commit, date
	if v != "dev" && c != "" {
		return v, c, d
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return v, c, d
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if c == "" && len(s.Value) >= 7 {
				c = s.Value[:7]
			}
		case "vcs.time":
			if d == "" && len(s.Value) >= 10 {
				d = s.Value[:10]
			}
		}
	}
	return v, c, d
}
