//go:build linux && !integration_runtime

package space

import (
	"os"
	"testing"

	"github.com/lalternativefabrique/sklp/space/spacenet"
)

// The re-exec'd test binary must be able to become a space worker or the
// subuid escalation child, as a real host binary does in main().
func TestMain(m *testing.M) {
	spacenet.MaybeRunWorker()
	spacenet.MaybeRunSubidRemove()
	os.Exit(m.Run())
}
