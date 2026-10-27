package spacemanager

import (
	"bufio"
	"context"
	"io"
	"os"
	"time"
)

// DumpLog reads the current contents of a service log file and calls emit for
// each complete line (no trailing newline), then returns. It is the one-shot
// `sklp deploy logs <svc>` read: no follow, no container touched. A missing
// file yields os.ErrNotExist so the caller can print "no logs yet".
func DumpLog(path string, emit func(string)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	partial, _ := readNewLines(r, "", emit)
	if partial != "" { // a final line with no trailing newline
		emit(partial)
	}
	return nil
}

// FollowLog tails path like `tail -f`, calling emit for each new whole line
// until ctx is cancelled. It tolerates the file not existing yet (polls until
// it appears). READ-ONLY: it never starts or stops a container. The poll
// interval mirrors the agent's logStreamer so follow latency is consistent.
func FollowLog(ctx context.Context, path string, emit func(string)) {
	fl := &lineFollower{path: path}
	defer fl.close()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		fl.poll(emit)
		select {
		case <-ctx.Done():
			fl.flush(emit)
			return
		case <-ticker.C:
		}
	}
}

// lineFollower is the cursor a `tail -f` keeps on one captured log file: the
// open handle, the bytes consumed so far and the unterminated tail carried
// between polls.
type lineFollower struct {
	path     string
	f        *os.File
	r        *bufio.Reader
	partial  string
	consumed int64
}

// poll opens the file once it exists, then emits every whole line appended
// since the last pass. The runtime reopens a re-run instance's file O_TRUNC,
// which keeps the inode: a size below what was consumed is the only trace, and
// the cursor rewinds to the start so the new run's output is read from its
// first byte.
func (l *lineFollower) poll(emit func(string)) {
	if l.f == nil {
		of, err := os.Open(l.path)
		if err != nil {
			return
		}
		l.f = of
		l.r = bufio.NewReader(of)
	}
	if l.consumed > 0 {
		if st, err := l.f.Stat(); err == nil && st.Size() < l.consumed {
			if _, err := l.f.Seek(0, io.SeekStart); err == nil {
				l.r.Reset(l.f)
				l.partial = ""
				l.consumed = 0
			}
		}
	}
	var n int64
	l.partial, n = readNewLines(l.r, l.partial, emit)
	l.consumed += n
}

// flush drains what is left and emits a final unterminated line, so a
// container that exits without a trailing newline still has its last words
// shown.
func (l *lineFollower) flush(emit func(string)) {
	if l.r == nil {
		return
	}
	var n int64
	l.partial, n = readNewLines(l.r, l.partial, emit)
	l.consumed += n
	if l.partial != "" {
		emit(l.partial)
		l.partial = ""
	}
}

func (l *lineFollower) close() {
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
}
