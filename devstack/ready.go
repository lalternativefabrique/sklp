package devstack

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/lalternativefabrique/sklp/pipeline"
)

// readyProbe waits until the service signals ready or timeout elapses.
// The four probe types are tried in priority order: HTTP, TCP, File,
// Log. Exactly one is honoured per service; the rest are ignored.
//
// `logTap` is consumed only by the Log probe — for every other probe
// the caller can leave the channel nil and the supervisor still drains
// stdout/stderr normally.
func readyProbe(ctx context.Context, r *pipeline.Ready, timeout time.Duration, logTap <-chan string) error {
	if r == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	switch {
	case r.HTTP != "":
		return waitHTTP(ctx, r.HTTP)
	case r.TCP != "":
		return waitTCP(ctx, r.TCP)
	case r.File != "":
		return waitFile(ctx, r.File)
	case r.Log != "":
		return waitLog(ctx, r.Log, logTap)
	default:
		return nil
	}
}

// waitHTTP polls the URL until any 2xx response. We use a short
// per-request timeout so a hung server doesn't eat the whole budget
// in a single GET.
func waitHTTP(ctx context.Context, url string) error {
	client := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13},
		},
	}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
		}
		if err := sleepOrDone(ctx, 500*time.Millisecond); err != nil {
			return fmt.Errorf("http probe %s: %w", url, err)
		}
	}
}

func waitTCP(ctx context.Context, hostPort string) error {
	d := net.Dialer{Timeout: 2 * time.Second}
	for {
		conn, err := d.DialContext(ctx, "tcp", hostPort)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if err := sleepOrDone(ctx, 500*time.Millisecond); err != nil {
			return fmt.Errorf("tcp probe %s: %w", hostPort, err)
		}
	}
}

func waitFile(ctx context.Context, path string) error {
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if err := sleepOrDone(ctx, 250*time.Millisecond); err != nil {
			return fmt.Errorf("file probe %s: %w", path, err)
		}
	}
}

func waitLog(ctx context.Context, pattern string, lines <-chan string) error {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("log probe regex: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("log probe %q: %w", pattern, ctx.Err())
		case line, ok := <-lines:
			if !ok {
				return fmt.Errorf("log probe %q: log channel closed before match", pattern)
			}
			if re.MatchString(line) {
				return nil
			}
		}
	}
}

func sleepOrDone(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// logBroadcaster fans out a service's log lines to N subscribers.
// Used so the supervisor can both print lines AND (optionally) feed
// them to a Log readiness probe without one consumer starving the
// other.
type logBroadcaster struct {
	mu   sync.Mutex
	subs []chan string
}

func newLogBroadcaster() *logBroadcaster {
	return &logBroadcaster{}
}

func (b *logBroadcaster) Subscribe() <-chan string {
	ch := make(chan string, 64)
	b.mu.Lock()
	b.subs = append(b.subs, ch)
	b.mu.Unlock()
	return ch
}

func (b *logBroadcaster) Publish(line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		// Non-blocking: a slow subscriber must not stall log printing.
		// Dropped lines for the readiness probe just mean we wait for
		// the next match; the user-facing log printer always wins.
		select {
		case ch <- line:
		default:
		}
	}
}

func (b *logBroadcaster) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		close(ch)
	}
	b.subs = nil
}
