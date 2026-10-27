package sklet

import (
	"os"
	"reflect"
	"testing"
)

// TestBuildSuperviseCommand asserts the detached __supervise command is built
// correctly WITHOUT executing it: argv, a new session (Setsid), stdin from the
// null device, and stdout/stderr both pointed at the log file (terminal freed).
func TestBuildSuperviseCommand(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	defer devNull.Close()
	logFile, err := os.CreateTemp(t.TempDir(), "supervisor-*.log")
	if err != nil {
		t.Fatalf("temp log: %v", err)
	}
	defer logFile.Close()

	cmd := BuildSuperviseCommand("/opt/sklet", "production", devNull, logFile)

	wantArgs := []string{"/opt/sklet", "__supervise", "--space", "production"}
	if !reflect.DeepEqual(cmd.Args, wantArgs) {
		t.Fatalf("argv = %v, want %v", cmd.Args, wantArgs)
	}
	if cmd.Path != "/opt/sklet" {
		t.Fatalf("path = %q, want /opt/sklet", cmd.Path)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatalf("SysProcAttr.Setsid must be true to detach the session, got %+v", cmd.SysProcAttr)
	}
	if cmd.Stdin != devNull {
		t.Fatalf("stdin must be the null device, got %v", cmd.Stdin)
	}
	if cmd.Stdout != logFile || cmd.Stderr != logFile {
		t.Fatalf("stdout/stderr must both be the log file (terminal freed): stdout=%v stderr=%v", cmd.Stdout, cmd.Stderr)
	}
}
