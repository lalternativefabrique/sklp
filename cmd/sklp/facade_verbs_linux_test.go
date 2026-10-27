//go:build linux

package main

import (
	"testing"

	"github.com/lalternativefabrique/sklp/internal/cli"
)

// The deploy verb declares its full flag/sub-verb surface only on Linux; the
// !linux stub is a RawArgs relay shell with nothing to parse.

func TestDeployLogsInterleavedPositional(t *testing.T) {
	logs := deployLogsCommand()
	for _, argv := range [][]string{{"db", "-f"}, {"-f", "db"}, {"-follow", "db"}} {
		a, err := cli.ParseLeaf(logs, argv)
		if err != nil {
			t.Fatalf("parse %v: %v", argv, err)
		}
		if got := a.Positionals(); len(got) != 1 || got[0] != "db" {
			t.Fatalf("%v positionals=%v", argv, got)
		}
		if !a.Bool("follow") {
			t.Fatalf("%v did not set follow", argv)
		}
	}
}

// TestDeployNetFlagGroup asserts the reusable net flag group (server/addr alias,
// token, ca-fingerprint, insecure) is present on a deploy sub-verb.
func TestDeployNetFlagGroup(t *testing.T) {
	ls := deployLsCommand()
	a, err := cli.ParseLeaf(ls, []string{"-addr", "https://h:9443", "-token", "tk", "-ca-fingerprint", "fp", "-insecure"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	nf := netFlagsFromArgs(a)
	if nf.server != "https://h:9443" || nf.token != "tk" || nf.caFinger != "fp" || !nf.insecure {
		t.Fatalf("net flags: %+v", nf)
	}
	if !nf.remote() {
		t.Fatalf("expected remote() true for --addr")
	}
}

// TestDeployBareStackPositional asserts the bare deploy flow tolerates a <stack>
// positional among its flags.
func TestDeployBareStackPositional(t *testing.T) {
	dep := deployCommand()
	a, err := cli.ParseLeaf(dep, []string{"mystack", "-space", "prod", "-force"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := firstArg(a, 0); got != "mystack" {
		t.Fatalf("stack=%q", got)
	}
	if a.String("space") != "prod" || !a.Bool("force") {
		t.Fatalf("deploy flags: %+v", a)
	}
}

// TestDeployHasNoSupervisorFlags locks the sklp/sklet split: the supervisor-bound
// flags (--foreground, --listen) drive a LOCAL supervisor, which is sklet's job.
// Their presence on `sklp deploy` would mean the client half is still trying to
// run a node.
func TestDeployHasNoSupervisorFlags(t *testing.T) {
	for _, f := range deployCommand().Flags {
		if f.Name == "foreground" || f.Name == "listen" {
			t.Fatalf("sklp deploy declares %q; supervisor flags belong to `sklet apply`", f.Name)
		}
	}
}

// TestDeployHasNoNodeSubverbs locks the same split on the verb tree: `agent` and
// the hidden `__supervise` daemon run ON a node, so they must not be reachable
// from the workstation client.
func TestDeployHasNoNodeSubverbs(t *testing.T) {
	for _, s := range deployCommand().Sub {
		if s.Name == "agent" || s.Name == "__supervise" {
			t.Fatalf("sklp deploy still exposes %q; it belongs to the sklet binary", s.Name)
		}
	}
}
