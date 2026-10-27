package control

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/lalternativefabrique/sklp/internal/xdg"
)

// AcquireSpaceLock takes an exclusive, non-blocking flock on the space's
// lock file (<cacheRoot>/deploy-locks/<space>.lock). It is the single-writer
// guard that makes a 2nd `sklp deploy --space X` impossible to start a
// COMPETING supervisor — even in the race window before the control socket is
// bound, or if a caller bypasses the socket. The held fd lives for the whole
// supervisor lifetime; the returned release closes it (dropping the lock) and
// removes the file, best-effort and idempotent.
//
// On contention it returns an error whose message names the space, so the CLI
// can exit cleanly (non-zero, no bridge created) instead of crash-looping on a
// double-bound port.
func AcquireSpaceLock(space string) (release func(), err error) {
	dir := filepath.Join(xdg.CacheRoot(), "deploy-locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create lock dir: %w", err)
	}
	path := filepath.Join(dir, space+".lock")

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if err == unix.EWOULDBLOCK {
			return nil, fmt.Errorf("space %q is already being supervised (another sklp deploy holds the lock)", space)
		}
		return nil, fmt.Errorf("lock space %q: %w", space, err)
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			// Drop the advisory lock, close the fd, then remove the file. Order is
			// best-effort: even if removal fails, the lock is released so a fresh
			// supervisor can re-acquire (it reuses the same file).
			_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
			_ = f.Close()
			_ = os.Remove(path)
		})
	}, nil
}
