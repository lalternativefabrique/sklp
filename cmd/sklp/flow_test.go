package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/pipeline"
)

func TestClassifyRunErr(t *testing.T) {
	t.Run("nil error passes", func(t *testing.T) {
		r := classifyRunErr("ci", nil)
		if r.state != pipelinePassed {
			t.Fatalf("want pipelinePassed, got %v", r.state)
		}
		if r.code != 0 {
			t.Fatalf("want code 0, got %d", r.code)
		}
	})

	t.Run("require violation skips", func(t *testing.T) {
		err := &pipeline.RequireViolationError{Message: "branch not allowed", Rule: pipeline.RequireRuleBranches}
		if !errors.Is(err, pipeline.ErrRequireViolation) {
			t.Fatal("precondition: RequireViolationError must wrap ErrRequireViolation")
		}
		r := classifyRunErr("sdk-publish", err)
		if r.state != pipelineSkipped {
			t.Fatalf("want pipelineSkipped, got %v", r.state)
		}
	})

	t.Run("wrapped require violation skips", func(t *testing.T) {
		err := fmt.Errorf("run failed: %w", pipeline.ErrRequireViolation)
		r := classifyRunErr("security-dast", err)
		if r.state != pipelineSkipped {
			t.Fatalf("want pipelineSkipped, got %v", r.state)
		}
	})

	t.Run("generic error fails with non-zero code", func(t *testing.T) {
		r := classifyRunErr("ci", errors.New("boom"))
		if r.state != pipelineFailed {
			t.Fatalf("want pipelineFailed, got %v", r.state)
		}
		if r.code == 0 {
			t.Fatalf("want non-zero code, got %d", r.code)
		}
	})
}

// `flow end` opens a PR by default and leaves CI to the PR. The flag surface is
// the contract users see, so it is asserted here: a stray --pr coming back, or
// --local losing its --test alias, silently changes what the command does.
func TestFlowEndFlagSurface(t *testing.T) {
	var end *cli.Command
	for i := range flowCommand().Sub {
		if flowCommand().Sub[i].Name == "end" {
			end = &flowCommand().Sub[i]
		}
	}
	if end == nil {
		t.Fatal("flow end is missing")
	}

	got := map[string][]string{}
	for _, f := range end.Flags {
		got[f.Name] = f.Aliases
	}

	if _, ok := got["pr"]; ok {
		t.Error("--pr must be gone: opening a PR is the default now")
	}
	aliases, ok := got["local"]
	if !ok {
		t.Fatal("--local is missing: it is the only way to run the pipeline here")
	}
	if len(aliases) == 0 || aliases[0] != "test" {
		t.Errorf("--local must keep its --test alias, got %v", aliases)
	}
	for _, want := range []string{"onto", "pipeline", "no-test"} {
		if _, ok := got[want]; !ok {
			t.Errorf("--%s is missing", want)
		}
	}
}
