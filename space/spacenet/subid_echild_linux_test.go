//go:build linux

package spacenet

import (
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

// The supervisor that maps subuids also runs a SIGCHLD subreaper doing
// Wait4(-1), which reaps every child — this helper included. os/exec then finds
// nothing to wait on and reports "waitid: no child processes", although the
// helper ran and its uid_map write already landed. Treating that as a failure
// took a space down at boot on a node whose mapping was correct.
//
// The test reproduces the theft: a goroutine reaps the child first, then
// runMapHelper must still report success.
func TestRunMapHelper_ToleratesAStolenExitStatus(t *testing.T) {
	true_, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no true(1) here")
	}

	stolen := make(chan struct{})
	go func() {
		defer close(stolen)
		// Race os/exec for the status; on this box either may win.
		for i := 0; i < 200; i++ {
			var ws unix.WaitStatus
			if pid, werr := unix.Wait4(-1, &ws, unix.WNOHANG, nil); pid > 0 && werr == nil {
				return
			}
		}
	}()

	out, err := runMapHelper(true_, nil)
	<-stolen

	if err != nil {
		t.Errorf("runMapHelper reported %v (out=%q); a stolen status must not fail the mapping", err, out)
	}
}

// A helper that genuinely fails must still fail: the ECHILD tolerance must not
// swallow a mapping that did not happen.
func TestRunMapHelper_StillFailsOnARealError(t *testing.T) {
	false_, err := exec.LookPath("false")
	if err != nil {
		t.Skip("no false(1) here")
	}
	if _, err := runMapHelper(false_, nil); err == nil {
		t.Error("a non-zero exit was reported as success")
	}
}
