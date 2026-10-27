package control

import (
	"fmt"
	"strings"
	"testing"
)

// The unit must refuse to restart on the configuration exit code. Without it a
// manifest this node cannot satisfy relaunches every RestartSec forever, and the
// unit reports "activating" — a state that reads as progress — instead of
// "failed" with the reason.
func TestRenderUnit_PreventsRestartOnConfigError(t *testing.T) {
	unit := renderUnit("production", "/usr/local/bin/sklet", nil)

	want := fmt.Sprintf("RestartPreventExitStatus=%d", ExitConfigError)
	if !strings.Contains(unit, want) {
		t.Errorf("unit does not carry %q, so a config error would crashloop:\n%s", want, unit)
	}
	// Retrying is still right for a transient failure.
	if !strings.Contains(unit, "Restart=on-failure") {
		t.Error("the unit no longer restarts on ordinary failures")
	}
}

// The exit code is shared between the supervisor that returns it and the unit
// that reacts to it; 78 is EX_CONFIG from sysexits(3).
func TestExitConfigError_IsEXCONFIG(t *testing.T) {
	if ExitConfigError != 78 {
		t.Errorf("ExitConfigError = %d, want 78 (EX_CONFIG)", ExitConfigError)
	}
}
