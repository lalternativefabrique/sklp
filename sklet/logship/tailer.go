package logship

import (
	"bufio"
	"context"
	"io"
	"os"
	"sync"
	"time"
)

// taggedSource is one instance's captured log file plus the service it
// belongs to, the unit reconcile() computes and sync() acts on.
type taggedSource struct {
	service string
	LogSource
}

// tailer follows a changing set of log files, emitting each new line via
// emit(service, line). sync() is the only mutation entry point: it starts
// tailing sources not yet tracked and stops those no longer present,
// keyed by file path (an instance's path is stable for its lifetime; a
// replacement instance gets a new path even if the label repeats).
type tailer struct {
	emit func(service, line string)

	mu      sync.Mutex
	cancels map[string]context.CancelFunc // path -> tail canceller
}

func newTailer(emit func(service, line string)) *tailer {
	return &tailer{emit: emit, cancels: make(map[string]context.CancelFunc)}
}

// sync makes the tracked set of tails exactly match sources: unseen paths
// start, vanished ones stop. Safe to call repeatedly (idempotent no-op when
// the set is unchanged).
func (t *tailer) sync(sources []taggedSource) {
	want := make(map[string]taggedSource, len(sources))
	for _, s := range sources {
		if s.Path != "" {
			want[s.Path] = s
		}
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	for path, cancel := range t.cancels {
		if _, ok := want[path]; !ok {
			cancel()
			delete(t.cancels, path)
		}
	}
	for path, s := range want {
		if _, ok := t.cancels[path]; ok {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		t.cancels[path] = cancel
		go t.tailLoop(ctx, path, s.service)
	}
}

// stopAll ends every tail goroutine (shutdown).
func (t *tailer) stopAll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for path, cancel := range t.cancels {
		cancel()
		delete(t.cancels, path)
	}
}

// tailPollInterval mirrors spacemanager's logStreamer: a captured file grows
// via plain appends, so a short poll is the simplest portable way to notice
// new bytes without an OS-specific inotify/kqueue dependency here.
const tailPollInterval = 200 * time.Millisecond

// tailLoop follows path from its start, emitting each new whole line until
// ctx is done. It tolerates the file not existing yet (a container that has
// not written its first line) by polling for it to appear.
func (t *tailer) tailLoop(ctx context.Context, path, service string) {
	var f *os.File
	defer func() {
		if f != nil {
			_ = f.Close()
		}
	}()

	var reader *bufio.Reader
	var partial string
	var consumed int64
	ticker := time.NewTicker(tailPollInterval)
	defer ticker.Stop()

	for {
		if f == nil {
			if of, err := os.Open(path); err == nil {
				f = of
				reader = bufio.NewReader(f)
			}
		}
		if reader != nil {
			if truncatedBelow(f, consumed) {
				if _, err := f.Seek(0, io.SeekStart); err == nil {
					reader.Reset(f)
					partial = ""
					consumed = 0
				}
			}
			partial, consumed = t.drain(reader, service, partial, consumed)
		}
		select {
		case <-ctx.Done():
			if reader != nil {
				partial, _ = t.drain(reader, service, partial, consumed)
			}
			if partial != "" {
				t.emit(service, partial)
			}
			return
		case <-ticker.C:
		}
	}
}

// truncatedBelow reports whether f shrank below what was already read: the
// runtime reopens a re-run instance's file O_TRUNC, which keeps the inode.
func truncatedBelow(f *os.File, consumed int64) bool {
	if consumed == 0 {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Size() < consumed
}

// drain reads every complete line currently available and emits each,
// returning the trailing partial to carry into the next pass and the running
// count of bytes consumed from the file.
func (t *tailer) drain(r *bufio.Reader, service, partial string, consumed int64) (string, int64) {
	for {
		chunk, err := r.ReadString('\n')
		consumed += int64(len(chunk))
		partial += chunk
		if len(partial) > 0 && partial[len(partial)-1] == '\n' {
			t.emit(service, partial[:len(partial)-1])
			partial = ""
		}
		if err != nil {
			return partial, consumed
		}
	}
}
