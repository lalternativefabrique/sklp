package pipeline

import (
	"os"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/plan"
)

// TestMain mirrors cmd/sklp's main(): plan.runSandboxed re-execs the test
// binary with the __sandbox-exec verb to apply Landlock in the child before
// running a step. The re-exec'd child is THIS test binary, so it must honour
// the verb too — otherwise it re-enters the test runner, spawns another
// sandboxed step, re-execs again, and the parent hangs forever in cmd.Wait()
// (two nested `runner.test __sandbox-exec ...` processes). internal/plan's own
// TestMain already does this; the runner package runs real pipelines through
// the same sandbox, so it needs the identical hook.
func TestMain(m *testing.M) {
	plan.MaybeRunSandboxShim()
	// Isolate the central CI-log store (CentralLogDir reads XDG_CACHE_HOME) so
	// RunPipeline tests never write run logs into the developer's real ~/.cache
	// — that pollution made the cockpit tail a stray test log (slug "001").
	tmp, err := os.MkdirTemp("", "sklp-pipeline-cache-")
	if err == nil {
		os.Setenv("XDG_CACHE_HOME", tmp)
	}
	code := m.Run()
	if tmp != "" {
		os.RemoveAll(tmp)
	}
	os.Exit(code)
}
