package reconcile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// ErrStateBusy is returned when another process already reconciles from this
// node's state: the agent and a hand-run `sklet reconcile` would otherwise
// both apply, and both roll the same services.
var ErrStateBusy = errors.New("this node's reconcile state is already open by another process (the agent reconciles, or a `sklet reconcile` runs)")

func lockState(cacheRoot string) (*os.File, error) {
	dir := filepath.Dir(StateDir(cacheRoot))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open reconcile lock: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrStateBusy
		}
		return nil, fmt.Errorf("lock reconcile state: %w", err)
	}
	return f, nil
}
