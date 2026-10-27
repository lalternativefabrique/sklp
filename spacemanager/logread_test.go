package spacemanager

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestDumpLog_ReadsWholeFile is the `sklp deploy logs <svc>` (no -f) guard:
// every complete line plus a final unterminated line is emitted once, in order.
func TestDumpLog_ReadsWholeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "service.log")
	if err := os.WriteFile(path, []byte("api up\nready\nlast-no-newline"), 0o600); err != nil {
		t.Fatal(err)
	}

	var got []string
	if err := DumpLog(path, func(l string) { got = append(got, l) }); err != nil {
		t.Fatalf("DumpLog: %v", err)
	}
	want := []string{"api up", "ready", "last-no-newline"}
	if len(got) != len(want) {
		t.Fatalf("DumpLog emitted %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestDumpLog_MissingFile returns os.ErrNotExist so the CLI can print
// "no logs yet" instead of a hard failure.
func TestDumpLog_MissingFile(t *testing.T) {
	err := DumpLog(filepath.Join(t.TempDir(), "absent.log"), func(string) {})
	if !os.IsNotExist(err) {
		t.Fatalf("DumpLog on missing file = %v, want os.ErrNotExist", err)
	}
}

// TestFollowLog_TailsUntilCancel is the `-f` guard: FollowLog emits the
// existing contents and lines appended afterwards, then returns when ctx is
// cancelled (no infinite loop). It also flushes a final unterminated line.
func TestFollowLog_TailsUntilCancel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "service.log")
	if err := os.WriteFile(path, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var got []string
	emit := func(l string) {
		mu.Lock()
		got = append(got, l)
		mu.Unlock()
	}
	has := func(want string) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, l := range got {
			if l == want {
				return true
			}
		}
		return false
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { FollowLog(ctx, path, emit); close(done) }()

	waitFor(t, 2*time.Second, "initial line", func() bool { return has("first") })

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("second\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	waitFor(t, 2*time.Second, "appended line", func() bool { return has("second") })

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("FollowLog did not return after ctx cancel (infinite tail)")
	}
}

// TestFollowLog_RewindsAfterTruncate is the job re-run guard: the runtime
// reopens the same instance's file O_TRUNC, and a shorter second run must
// still be emitted from its first byte instead of being read past.
func TestFollowLog_RewindsAfterTruncate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "service.log")
	if err := os.WriteFile(path, []byte("51/u tenant_vat_franchise (23ms)\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var got []string
	emit := func(l string) {
		mu.Lock()
		got = append(got, l)
		mu.Unlock()
	}
	has := func(want string) func() bool {
		return func() bool {
			mu.Lock()
			defer mu.Unlock()
			for _, l := range got {
				if l == want {
					return true
				}
			}
			return false
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		FollowLog(ctx, path, emit)
		close(done)
	}()
	waitForCond(t, 2*time.Second, "first run line", has("51/u tenant_vat_franchise (23ms)"))

	if err := os.WriteFile(path, []byte("no change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, 2*time.Second, "second run line after truncate", has("no change"))

	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("emitted %v, want exactly the two runs", got)
	}
}

func waitForCond(t *testing.T, d time.Duration, msg string, cond func() bool) {
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
