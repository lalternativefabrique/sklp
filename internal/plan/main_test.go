package plan

import (
	"os"
	"testing"
)

// TestMain mirrors cmd/sklp's main(): runSandboxed re-execs the test
// binary with the __sandbox-exec verb to apply Landlock in the child, so
// the test binary must honour that verb too. Without this hook the
// re-exec'd child would re-enter the test runner and hang in cmd.Wait().
func TestMain(m *testing.M) {
	MaybeRunSandboxShim()
	// Isolate XDG_CACHE_HOME so pipeline/CI tests never write run logs or caches
	// into the developer's real ~/.cache (see pipeline.CentralLogDir).
	tmp, err := os.MkdirTemp("", "sklp-plan-cache-")
	if err == nil {
		os.Setenv("XDG_CACHE_HOME", tmp)
	}
	code := m.Run()
	if tmp != "" {
		os.RemoveAll(tmp)
	}
	os.Exit(code)
}
