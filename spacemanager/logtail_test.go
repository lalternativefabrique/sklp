package spacemanager

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeLogProvider maps an id to a fixed file path for the streamer.
type fakeLogProvider struct{ paths map[string]string }

func (f fakeLogProvider) LogPath(id string) string { return f.paths[id] }

// collectSink is a thread-safe LogSink that records (service, line) pairs.
type collectSink struct {
	mu    sync.Mutex
	lines []string
}

func (c *collectSink) sink() LogSink {
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

// TestLogStreamer_TailsAndPrefixes is the FIX-2 guard: lines appended to a
// service's captured log are tailed, prefixed by the service, and emitted —
// including a line written AFTER the tail started (the `tail -f` behaviour) and
// a final unterminated line on stop.
func TestLogStreamer_TailsAndPrefixes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "service.log")
	if err := os.WriteFile(path, []byte("db up\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := &collectSink{}
	s := newLogStreamer(c.sink(), fakeLogProvider{paths: map[string]string{"db-0": path}})
	if !s.enabled() {
		t.Fatal("streamer should be enabled with a sink and provider")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.start(ctx, "db-0", "db")

	waitFor(t, 2*time.Second, "log line", func() bool {
		for _, l := range c.snapshot() {
			if l == "[db] db up" {
				return true
			}
		}
		return false
	})

	// Append a line after the tail is following: it must be picked up.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("DB-PAYLOAD-OK\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	waitFor(t, 2*time.Second, "log line", func() bool {
		for _, l := range c.snapshot() {
			if l == "[db] DB-PAYLOAD-OK" {
				return true
			}
		}
		return false
	})

	s.stop("db-0")

	// recentLines retains the tail for the failure summary.
	recent := s.recentLines("db")
	if len(recent) < 2 || recent[len(recent)-1] != "DB-PAYLOAD-OK" {
		t.Fatalf("recentLines = %v, want last 'DB-PAYLOAD-OK'", recent)
	}
}

// TestLogStreamer_DisabledNoProvider confirms a nil provider/sink is a safe
// no-op (the exec test launcher path): no panic, no emission.
func TestLogStreamer_DisabledNoProvider(t *testing.T) {
	s := newLogStreamer(nil, nil)
	if s.enabled() {
		t.Fatal("streamer must be disabled without a sink/provider")
	}
	// All operations are safe no-ops.
	s.start(context.Background(), "x", "x")
	s.stop("x")
	s.stopAll()
	if got := s.recentLines("x"); got != nil {
		t.Fatalf("recentLines on disabled streamer = %v, want nil", got)
	}
}

// TestLogStreamer_RingCaps confirms the per-service recent ring is bounded so a
// chatty service does not grow the buffer without limit.
func TestLogStreamer_RingCaps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "service.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c := &collectSink{}
	s := newLogStreamer(c.sink(), fakeLogProvider{paths: map[string]string{"api-0": path}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.start(ctx, "api-0", "api")

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxRecentLines*3; i++ {
		if _, err := f.WriteString("line\n"); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.Close()

	waitFor(t, 2*time.Second, "log line", func() bool {
		return len(c.snapshot()) >= maxRecentLines*3
	})
	s.stop("api-0")

	if got := len(s.recentLines("api")); got != maxRecentLines {
		t.Fatalf("recent ring len = %d, want capped at %d", got, maxRecentLines)
	}
}

// TestLogStreamer_RunLinesAreScopedToTheRun is the regression guard for a
// oneshot whose recorded tail came from the run BEFORE it: the per-service ring
// spans runs, so a re-run that printed different lines was reported with its
// predecessor's output — a migration log showing the previous version's applied
// steps, which reads as if the new one never ran.
func TestLogStreamer_RunLinesAreScopedToTheRun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "service.log")
	if err := os.WriteFile(path, []byte("26/u user_identity_id\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := &collectSink{}
	s := newLogStreamer(c.sink(), fakeLogProvider{paths: map[string]string{"migrate-0": path}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.start(ctx, "migrate-0", "migrate")
	waitFor(t, 2*time.Second, "first run line tailed", func() bool {
		return len(s.recentLines("migrate")) == 1
	})
	s.stop("migrate-0")

	// The runtime reopens the file O_TRUNC for the next launch: this run's
	// output is the only thing in it.
	if err := os.WriteFile(path, []byte("27/u jwks\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.start(ctx, "migrate-0", "migrate")
	defer s.stop("migrate-0")

	got := s.runLines("migrate-0")
	if len(got) != 1 || got[0] != "27/u jwks" {
		t.Fatalf("runLines = %v, want only this run's [27/u jwks]", got)
	}
	// The cross-run failure summary still spans runs, by design.
	if recent := s.recentLines("migrate"); len(recent) == 0 || recent[0] != "26/u user_identity_id" {
		t.Fatalf("per-service ring = %v, want it to still span both runs", recent)
	}
}

// TestLogStreamer_RunLinesBeatThePollInterval: a job that writes and exits
// inside one tailer tick printed lines the ring never buffered. The record must
// still carry them — the file already holds the whole run.
func TestLogStreamer_RunLinesBeatThePollInterval(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "service.log")
	if err := os.WriteFile(path, []byte("no change\nno change\n27/u jwks\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := &collectSink{}
	s := newLogStreamer(c.sink(), fakeLogProvider{paths: map[string]string{"migrate-0": path}})

	// No start() at all: nothing was ever tailed, which is exactly what happens
	// to a oneshot that exits during Watch. Reading must still work off the file.
	got := s.runLines("migrate-0")
	if len(got) != 3 || got[2] != "27/u jwks" {
		t.Fatalf("runLines = %v, want the whole run read from the file", got)
	}
	_ = c
}
