package agentdetect

import (
	"testing"

	"github.com/lalternativefabrique/sklp/internal/skillgen"
)

func TestHeadlessCommand_Claude(t *testing.T) {
	a, ok := AgentForTarget("claude")
	if !ok {
		t.Fatal("claude not in catalog")
	}
	// Claude defines a headless mode; whether HeadlessCommand returns ok
	// depends on the `claude` binary being on PATH in this environment.
	if a.headlessArgs == nil {
		t.Fatal("claude should define headlessArgs")
	}
	got := a.headlessArgs("do the thing")
	want := []string{"-p", "--verbose", "do the thing"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("headlessArgs = %v, want %v", got, want)
	}
}

func TestHeadlessArgs_KnownAgents(t *testing.T) {
	cases := map[skillgen.Target][]string{
		"claude": {"-p", "--verbose", "P"},
		"codex":  {"exec", "P"},
		"vibe":   {"-p", "P"},
	}
	for target, want := range cases {
		a, ok := AgentForTarget(target)
		if !ok {
			t.Fatalf("%s not in catalog", target)
		}
		if a.headlessArgs == nil {
			t.Fatalf("%s should define headlessArgs", target)
		}
		got := a.headlessArgs("P")
		if len(got) != len(want) || got[0] != want[0] || got[len(got)-1] != want[len(want)-1] {
			t.Errorf("%s headlessArgs = %v, want %v", target, got, want)
		}
	}
}

func TestHeadlessArgs_UnwiredAgents(t *testing.T) {
	// cursor/zed/cline have no verified headless CLI yet — they must stay
	// nil so init falls back to the manual hint rather than running a
	// made-up command.
	for _, target := range []skillgen.Target{"cursor", "zed", "cline"} {
		a, ok := AgentForTarget(target)
		if !ok {
			t.Fatalf("%s not in catalog", target)
		}
		if a.headlessArgs != nil {
			t.Errorf("%s should NOT define headlessArgs (no verified headless mode)", target)
		}
	}
}

func TestHeadlessCommand_NoHeadlessAgent(t *testing.T) {
	// An agent without headlessArgs must report ok=false regardless of
	// PATH, so init falls back to the manual hint.
	a := Agent{Target: "zed", Label: "Zed", binaries: []string{"zed"}}
	if _, _, ok := a.HeadlessCommand("x"); ok {
		t.Error("agent with nil headlessArgs should not be runnable headless")
	}
}

func TestAgentForTarget_Unknown(t *testing.T) {
	if _, ok := AgentForTarget("nope"); ok {
		t.Error("unknown target should return ok=false")
	}
}
