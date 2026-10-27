package cockpit

import (
	"strings"
	"testing"
)

func floodedPanel(lines int) *Model {
	m := &Model{Width: 100, Height: 40, LogsOpen: true, LogsService: "postgres"}
	for i := 0; i < lines; i++ {
		m.AppendLogLine("[postgres] RETURNING id, user_id, prompt, repo_url, base_ref, mcp_servers, tools, system_prompt, report_diff, open_pr, pr_title, parent_id, head_ref, pull_request_url, status, model")
	}
	return m
}

// G jumps straight back to the tail from anywhere, in ONE press: on a service
// repeating a line, scrolling down to catch up never terminates.
func TestEndKeyReachesTailInOnePress(t *testing.T) {
	m := floodedPanel(1000)
	m.LogsScroll = 0

	if !m.Update(KeyEnd) {
		t.Fatal("G must act")
	}
	if m.LogsScroll != -1 {
		t.Fatalf("LogsScroll=%d want -1 (follow)", m.LogsScroll)
	}
}

// g jumps to the oldest row in one press.
func TestHomeKeyReachesTopInOnePress(t *testing.T) {
	m := floodedPanel(1000)
	m.LogsScroll = -1

	m.Update(KeyHome)
	if m.LogsScroll != 0 {
		t.Fatalf("LogsScroll=%d want 0 (top)", m.LogsScroll)
	}
}

// One ↓ press must advance at least a full log line. Stepping by one SCREEN row
// meant a wrapped line took several presses to clear and crossing the buffer
// took hundreds — which is what made the arrows read as broken.
func TestScrollStepAdvancesWholeLines(t *testing.T) {
	m := floodedPanel(200)
	rows, lines := len(m.logRows()), len(m.LogsLines)
	if rows <= lines {
		t.Fatalf("test needs wrapped lines: rows=%d lines=%d", rows, lines)
	}
	wrap := rows / lines

	if got := m.logsStep(); got < wrap {
		t.Fatalf("step=%d must cover at least one wrapped line (%d rows)", got, wrap)
	}
}

// A step never exceeds a page: ↓ must stay finer than PgDn.
func TestScrollStepNeverExceedsPage(t *testing.T) {
	for _, n := range []int{10, 200, 1000} {
		m := floodedPanel(n)
		if step, page := m.logsStep(), m.logsPage(); step > page {
			t.Fatalf("%d lines: step=%d must not exceed page=%d", n, step, page)
		}
	}
}

// PgDn crosses the whole buffer in a handful of presses — the unit for actually
// traversing a log, which ↓ is not.
func TestPageDownCrossesBufferQuickly(t *testing.T) {
	m := floodedPanel(1000)
	m.LogsScroll = 0

	presses := 0
	for m.LogsScroll >= 0 && presses < 1000 {
		m.Update(KeyPageDown)
		presses++
	}
	if presses > 70 {
		t.Fatalf("PgDn took %d presses to cross the buffer", presses)
	}
}

// The panel says whether it is following or paused: on a service repeating the
// same line, a paused view and a live one are otherwise indistinguishable.
func TestPanelReportsFollowState(t *testing.T) {
	m := floodedPanel(200)

	m.LogsScroll = -1
	if out := Render(m, NewPlainRenderer()); !strings.Contains(out, "following") {
		t.Fatalf("following state must be shown:\n%s", out)
	}

	m.LogsScroll = 10
	out := Render(m, NewPlainRenderer())
	if !strings.Contains(out, "paused") || !strings.Contains(out, "[G] follow") {
		t.Fatalf("paused state must be shown with the way back:\n%s", out)
	}
}
