package devstack

import (
	"os/exec"
	"testing"
	"time"
)

func TestReapExitStatusReadsTheChildsCode(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 3")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	code, known := reapExitStatus(cmd.Process.Pid)
	if !known || code != 3 {
		t.Fatalf("reapExitStatus = (%d, %v), want (3, true)", code, known)
	}
	if _, known := reapExitStatus(cmd.Process.Pid); known {
		t.Fatal("a reaped pid must not be reported twice")
	}
}
