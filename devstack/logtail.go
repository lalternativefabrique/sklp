package devstack

import (
	"bufio"
	"context"
	"io"
	"os"
	"time"
)

// tailLog follows a container's host-side log file (the launcher captures the
// service's stdout/stderr there) and publishes each new line to the
// broadcaster, so the embedded path feeds the SAME logPrinter + Log-readiness
// machinery the legacy proc path uses. It tolerates the file not existing yet
// (the launcher creates it as the container starts) by retrying, and returns
// when ctx is cancelled (teardown). The broadcaster is NOT closed here — a
// service crash must not race the printer; the supervisor owns broadcaster
// lifetime.
func tailLog(ctx context.Context, path string, b *logBroadcaster) {
	f := openLogWithRetry(ctx, path)
	if f == nil {
		return
	}
	defer f.Close()

	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			// Trim the trailing newline; the printer adds its own.
			if line[len(line)-1] == '\n' {
				line = line[:len(line)-1]
			}
			b.Publish(line)
			continue
		}
		if err != nil && err != io.EOF {
			return
		}
		// EOF: wait for more output (the container is still writing) unless
		// teardown asked us to stop.
		select {
		case <-ctx.Done():
			return
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// openLogWithRetry waits for the log file to appear (the launcher creates it as
// the container boots), giving up when ctx is cancelled. Returns nil on
// cancellation so the caller stops cleanly.
func openLogWithRetry(ctx context.Context, path string) *os.File {
	for {
		if f, err := os.Open(path); err == nil {
			return f
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(100 * time.Millisecond):
		}
	}
}
