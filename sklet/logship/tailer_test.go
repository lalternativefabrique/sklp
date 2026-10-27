package logship

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type collectSink struct {
	mu    sync.Mutex
	lines []string
}

func (c *collectSink) emit() func(service, line string) {
	return func(service, line string) {
		c.mu.Lock()
		c.lines = append(c.lines, "["+service+"] "+line)
		c.mu.Unlock()
	}
}

func (c *collectSink) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.lines...)
}

func waitFor(t *testing.T, d time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", msg)
}

// TestTailer_TailsAndPrefixes mirrors spacemanager's logStreamer guard: lines
// present at start and appended after (`tail -f`) are both emitted, tagged
// with the owning service.
func TestTailer_TailsAndPrefixes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "service.log")
	if err := os.WriteFile(path, []byte("db up\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := &collectSink{}
	tl := newTailer(c.emit())
	tl.sync([]taggedSource{{service: "db", LogSource: LogSource{Label: "db-0", Path: path}}})
	defer tl.stopAll()

	waitFor(t, 2*time.Second, "initial line", func() bool {
		for _, l := range c.snapshot() {
			if l == "[db] db up" {
				return true
			}
		}
		return false
	})

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("DB-PAYLOAD-OK\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	waitFor(t, 2*time.Second, "appended line", func() bool {
		for _, l := range c.snapshot() {
			if l == "[db] DB-PAYLOAD-OK" {
				return true
			}
		}
		return false
	})
}

// TestTailer_SyncStopsVanishedSources confirms a source dropped from a later
// sync() call stops being tailed — a service removed or an instance replaced
// must not keep emitting from its old file.
func TestTailer_SyncStopsVanishedSources(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "service.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	c := &collectSink{}
	tl := newTailer(c.emit())
	tl.sync([]taggedSource{{service: "api", LogSource: LogSource{Label: "api-0", Path: path}}})

	tl.mu.Lock()
	_, tracked := tl.cancels[path]
	tl.mu.Unlock()
	if !tracked {
		t.Fatal("path must be tracked right after sync")
	}

	tl.sync(nil)

	tl.mu.Lock()
	_, stillTracked := tl.cancels[path]
	tl.mu.Unlock()
	if stillTracked {
		t.Fatal("path must stop being tracked once absent from a later sync")
	}
}

// TestTailer_EmitsFinalUnterminatedLineOnStop mirrors logStreamer's guard: a
// container that exits mid-line still has its last words shipped.
func TestTailer_EmitsFinalUnterminatedLineOnStop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "service.log")
	if err := os.WriteFile(path, []byte("first\nno-newline-tail"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := &collectSink{}
	tl := newTailer(c.emit())
	tl.sync([]taggedSource{{service: "web", LogSource: LogSource{Label: "web-0", Path: path}}})

	waitFor(t, 2*time.Second, "first line", func() bool {
		for _, l := range c.snapshot() {
			if l == "[web] first" {
				return true
			}
		}
		return false
	})

	tl.stopAll()

	waitFor(t, 2*time.Second, "trailing unterminated line", func() bool {
		for _, l := range c.snapshot() {
			if l == "[web] no-newline-tail" {
				return true
			}
		}
		return false
	})
}

// TestTailer_RewindsAfterTruncate is the job re-run guard: a re-run keeps its
// instance id, so its path, and the runtime reopens the file O_TRUNC. A second
// run shorter than the first must still ship from its first byte.
func TestTailer_RewindsAfterTruncate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "service.log")
	if err := os.WriteFile(path, []byte("51/u tenant_vat_franchise (23ms)\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := &collectSink{}
	tl := newTailer(c.emit())
	tl.sync([]taggedSource{{service: "migrate", LogSource: LogSource{Label: "migrate-0", Path: path}}})
	defer tl.stopAll()

	has := func(want string) func() bool {
		return func() bool {
			for _, l := range c.snapshot() {
				if l == want {
					return true
				}
			}
			return false
		}
	}
	waitFor(t, 2*time.Second, "first run line", has("[migrate] 51/u tenant_vat_franchise (23ms)"))

	if err := os.WriteFile(path, []byte("no change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "second run line after truncate", has("[migrate] no change"))

	if got := c.snapshot(); len(got) != 2 {
		t.Fatalf("emitted %v, want exactly the two runs", got)
	}
}
