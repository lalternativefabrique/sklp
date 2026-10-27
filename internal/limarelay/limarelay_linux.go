//go:build linux

package limarelay

import (
	"errors"
	"fmt"
)

func RelaySelf(_ string) error {
	return errors.New("this host cannot run rootless runc (unprivileged user namespaces required)")
}

func VMRunning() bool { return false }

// RelayCommand only exists off Linux: on Linux there is no VM to relay into.
func RelayCommand(_ string, _ []string) error {
	return errors.New("no VM to relay into on a Linux host")
}

// FetchSktLinux is only needed off Linux (the vz rootfs build); a Linux host
// builds guests with the skt already on its own PATH or via -skt-bin.
func FetchSktLinux(_ string) (string, error) {
	return "", errors.New("not applicable on a Linux host")
}

// GuestExitError carries a guest exit status back to main, which turns it
// into this process's own status so a relayed run is indistinguishable from a
// local one.
type GuestExitError struct{ Code int }

func (e *GuestExitError) Error() string {
	return fmt.Sprintf("guest exited with status %d", e.Code)
}
