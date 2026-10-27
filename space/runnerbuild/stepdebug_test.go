package runnerbuild

import (
	"strings"
	"testing"
)

// The dump exists to diagnose which `go` a step selects, so it must reach the
// runc script too: instrumenting only one runner leaves the failing path
// blind, which is how the PATH ordering bug stayed invisible for so long.
func TestStepEnvDebugReachesTheRuncScript(t *testing.T) {
	t.Setenv(StepEnvDebugVar, "1")

	script := RuncStepRunner{}.buildScript([]string{"go build ./..."})

	if !strings.Contains(script, "[sklp stepenv]") {
		t.Fatalf("the env dump never reached the generated script:\n%s", script)
	}
	if !strings.Contains(script, "go build ./...") {
		t.Fatal("the step itself must still run after the dump")
	}
	if strings.Index(script, "[sklp stepenv]") > strings.Index(script, "go build ./...") {
		t.Fatal("the dump must print before the step, or a failing step hides it")
	}
}

func TestStepEnvDebugIsOffByDefault(t *testing.T) {
	t.Setenv(StepEnvDebugVar, "")

	if got := stepEnvDebugPrefix(); got != "" {
		t.Fatalf("the dump must stay out of normal runs, got %q", got)
	}
	if strings.Contains(RuncStepRunner{}.buildScript([]string{"true"}), "stepenv") {
		t.Fatal("a normal run must not carry the dump")
	}
}
