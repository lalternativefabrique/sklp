//go:build !linux

package main

import (
	"testing"

	"github.com/lalternativefabrique/sklp/internal/cli"
)

// The relayed space verb must not parse the guest's flags. Under Passthrough
// the host flagset claimed any token starting with "-" before the `--`
// separator, so `space exec <name> -c web` died on the host with "flag provided
// but not defined: -c" and never reached the VM.
func TestSpaceRelaysFlagsVerbatim(t *testing.T) {
	if !spaceCommand().RawArgs {
		t.Fatal("space must be RawArgs off Linux so guest flags relay unparsed")
	}
}

func TestSpaceNameCheckReadsRawTail(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want int
	}{
		{"bare verb needing a name", []string{"space", "exec"}, 2},
		{"bare down", []string{"space", "down"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cli.NewFlagAdapter().Execute(t.Context(), rootCommand(), tc.argv); got != tc.want {
				t.Fatalf("exit = %d, want %d", got, tc.want)
			}
		})
	}
}
