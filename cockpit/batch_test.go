package cockpit

import (
	"fmt"
	"testing"
)

// Opening a panel replays the service's whole history. Delivering it one line
// per message meant one redraw per line — thousands of frames of auto-scroll
// before the panel settled. A burst must cost a handful of redraws instead.
func TestHistoryBurstCostsFewRedraws(t *testing.T) {
	const history = 5000
	lines := make(chan string, history)
	for i := 0; i < history; i++ {
		lines <- fmt.Sprintf("[postgres] line %d", i)
	}
	close(lines)
	errs := make(chan string, 1)

	m := &Model{Width: 100, Height: 40, LogsOpen: true, LogsService: "postgres"}
	redraws, delivered := 0, 0
	cmd := waitLogLine(lines, errs)
	for cmd != nil {
		msg := cmd()
		seq, ok := msg.(seqMsg)
		if !ok {
			break
		}
		batch, ok := seq.msg.(logLineMsg)
		if !ok {
			t.Fatalf("got %T want logLineMsg", seq.msg)
		}
		redraws++
		delivered += len(batch.lines)
		for _, l := range batch.lines {
			m.AppendLogLine(l)
		}
		cmd = seq.next
	}

	if delivered != history {
		t.Fatalf("delivered %d lines, want %d — batching must not drop any", delivered, history)
	}
	if redraws > 20 {
		t.Fatalf("%d redraws for %d lines: the burst is still one-per-line", redraws, history)
	}
}

// However long the history, the panel settles on the last logBufferCap lines
// rather than growing without bound.
func TestHistoryTrimsToBufferCap(t *testing.T) {
	m := &Model{Width: 100, Height: 40, LogsOpen: true, LogsService: "postgres"}
	for i := 0; i < logBufferCap*4; i++ {
		m.AppendLogLine(fmt.Sprintf("line %d", i))
	}
	if len(m.LogsLines) != logBufferCap {
		t.Fatalf("buffer=%d want %d", len(m.LogsLines), logBufferCap)
	}
	want := fmt.Sprintf("line %d", logBufferCap*4-1)
	if got := m.LogsLines[len(m.LogsLines)-1]; got != want {
		t.Fatalf("last line %q want %q — the TAIL must survive, not the head", got, want)
	}
}

// A batch never exceeds the cap, so a fast producer cannot starve key handling.
func TestBatchIsBounded(t *testing.T) {
	lines := make(chan string, logBatchCap*3)
	for i := 0; i < logBatchCap*3; i++ {
		lines <- "x"
	}
	errs := make(chan string, 1)

	seq, ok := waitLogLine(lines, errs)().(seqMsg)
	if !ok {
		t.Fatal("want a seqMsg")
	}
	batch := seq.msg.(logLineMsg)
	if len(batch.lines) > logBatchCap {
		t.Fatalf("batch=%d exceeds cap %d", len(batch.lines), logBatchCap)
	}
}
