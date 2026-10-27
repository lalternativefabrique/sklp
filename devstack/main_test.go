package devstack

import (
	"os"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/plan"
)

// TestMain mirrors cmd/sklp's main(): a pipeline step runs through the
// internal/plan sandbox, which re-execs THIS binary with the
// __sandbox-exec verb to apply Landlock in the child before exec'ing the
// real command. The test binary must honour that verb too — otherwise the
// re-exec'd child re-enters the test runner, reboots the service, re-runs
// the step, and recurses until the context times out (observed as
// `step "see-service" (exit -1)` after 15s in
// TestRunPipeline_WithServices_EndToEnd).
//
// internal/plan has the same hook in its own main_test.go; devstack needs
// it because its E2E test drives runner.RunPipeline, which reaches the
// same sandbox shim.
func TestMain(m *testing.M) {
	plan.MaybeRunSandboxShim()
	os.Exit(m.Run())
}
