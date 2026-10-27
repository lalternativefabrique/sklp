package control

import (
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/xdg"
)

// The data root belongs to the NODE, not to a space. A unit written without it
// comes back after a reboot refusing to run a space with volumes — on a node
// that stores them perfectly well — because the supervisor no longer sees where.
func TestRenderUnit_CarriesTheNodeDataRoot(t *testing.T) {
	t.Setenv(xdg.DataRootEnv, "/data/sklp")

	unit := renderUnit("production", "/usr/local/bin/sklet", nil)

	want := "Environment=" + xdg.DataRootEnv + "=/data/sklp"
	if !strings.Contains(unit, want) {
		t.Errorf("unit does not carry %q, so the space would refuse its volumes:\n%s", want, unit)
	}
}

// A node with no data root writes no such line: the unit stays what it was
// before the field existed, and the supervisor refuses volumes for the right
// reason rather than reading an empty path.
func TestRenderUnit_OmitsAnAbsentDataRoot(t *testing.T) {
	t.Setenv(xdg.DataRootEnv, "")

	if unit := renderUnit("production", "/usr/local/bin/sklet", nil); strings.Contains(unit, xdg.DataRootEnv) {
		t.Errorf("unit mentions %s with none configured:\n%s", xdg.DataRootEnv, unit)
	}
}
