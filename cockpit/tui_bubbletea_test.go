package cockpit

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// joinCells used to trim its trailing padding, so a styled row was shorter than
// the table: the selection background stopped after the last cell and restarted
// past a visible gap. Every rendered line must span the full width in one piece.
func TestTableRowsAreFullWidth(t *testing.T) {
	const width = 120
	r := NewLipglossRenderer()
	cols := []Column{{Header: "", Width: 1}, {Header: "SPACE", Width: 20}, {Header: "REPO", Width: 30}}
	rows := []Row{
		{Cells: []string{"●", "ci-acme", "/home/u/code/projects/acme"}, Selected: true},
		{Cells: []string{"○", "sklp-dev", "/home/u/code/projects/waves"}},
	}
	for i, line := range strings.Split(r.Table(cols, rows, width), "\n") {
		if got := lipgloss.Width(line); got != width {
			t.Errorf("line %d is %d wide, want %d — background would break mid-row", i, got, width)
		}
	}
}

// A cell that carries its own color closes it with a reset, which cut the
// selection background mid-row and left a stray block around the status glyph.
// The row must be one uninterrupted styled span.
func TestSelectedRowHasNoMidLineReset(t *testing.T) {
	// Styling is a no-op under the test binary's Ascii profile, so the escape
	// sequences this asserts on only exist when colors are forced on.
	defer lipgloss.SetColorProfile(lipgloss.ColorProfile())
	lipgloss.SetColorProfile(termenv.TrueColor)
	r := NewLipglossRenderer()
	cols := []Column{{Header: "", Width: 1}, {Header: "SPACE", Width: 22}}
	rows := []Row{{
		Cells:    []string{r.StatusGlyph(StateReady), "sklp-dev-f09cf69923a2"},
		State:    StateReady,
		Selected: true,
	}}
	line := strings.Split(r.Table(cols, rows, 60), "\n")[1]

	// Every visible character must sit inside the reverse-video span: a reset
	// that is not immediately followed by re-opening it leaves the rest of the
	// row unhighlighted, which is the stray-block bug.
	const reverse = "7"
	for _, seg := range strings.SplitAfter(line, "\x1b[0m") {
		text := strings.TrimSuffix(seg, "\x1b[0m")
		if stripANSI(text) == "" {
			continue
		}
		if !hasSGRParam(text, reverse) {
			t.Fatalf("segment %q is not reversed — the row breaks mid-line:\n%q", text, line)
		}
	}
}

// hasSGRParam reports whether the segment opens with an SGR sequence carrying
// the given parameter (e.g. "7" for reverse video).
func hasSGRParam(seg, param string) bool {
	i := strings.Index(seg, "\x1b[")
	if i < 0 {
		return false
	}
	j := strings.Index(seg[i:], "m")
	if j < 0 {
		return false
	}
	for _, p := range strings.Split(seg[i+2:i+j], ";") {
		if p == param {
			return true
		}
	}
	return false
}

// A bordered block must be rendered AT its target width, never clipped after the
// fact: trimming a rendered border shears it mid-line and leaves fragments.
func TestBoxRendersAtTheRequestedWidth(t *testing.T) {
	r := NewLipglossRenderer()
	for _, width := range []int{40, 80, 150} {
		for i, line := range strings.Split(r.Box("title", "body", width), "\n") {
			if got := lipgloss.Width(line); got != width {
				t.Errorf("width %d: line %d is %d wide, border would break", width, i, got)
			}
		}
	}
}

// mapKey lowercased before matching, so `case "D"` was unreachable and Shift-D
// (purge every idle space) landed on `d` (delete the selected one).
func TestMapKeyDistinguishesDeleteFromPurge(t *testing.T) {
	if got := mapKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}}); got != KeyKill {
		t.Errorf("d must delete the selected space, got %v", got)
	}
	if got := mapKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'D'}}); got != KeyPurge {
		t.Errorf("D must purge idle spaces, got %v", got)
	}
}

func TestMapKeyCopy(t *testing.T) {
	if got := mapKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}}); got != KeyCopy {
		t.Errorf("c must copy the log buffer, got %v", got)
	}
}

// SideBySide aligns the right column on the widest left line, so an overflowing
// left line pushed the logs pane off-screen. Every left line must be clamped.
func TestSideBySideClampsAnOverflowingLeftLine(t *testing.T) {
	const leftWidth = 20
	r := NewLipglossRenderer()
	left := "short\n" + strings.Repeat("x", 80)
	out := r.SideBySide(left, "LOGS\nLOGS", leftWidth)
	for i, line := range strings.Split(out, "\n") {
		if got, want := lipgloss.Width(line), leftWidth+splitGutter+4; got != want {
			t.Errorf("line %d is %d wide, want %d — the logs column would be shifted off-screen", i, got, want)
		}
	}
}

// s is the stop action; sort moved to o. An action on a running stack deserves
// the direct key, a display setting does not.
func TestMapKeyStopAndOrder(t *testing.T) {
	cases := map[string]Key{
		"s": KeyStop,
		"o": KeySort,
		"g": KeyHome,
		"G": KeyEnd,
	}
	for in, want := range cases {
		if got := mapKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(in)}); got != want {
			t.Errorf("%q mapped to %v, want %v", in, got, want)
		}
	}
	if got := mapKey(tea.KeyMsg{Type: tea.KeyPgUp}); got != KeyPageUp {
		t.Errorf("PgUp mapped to %v", got)
	}
	if got := mapKey(tea.KeyMsg{Type: tea.KeyPgDown}); got != KeyPageDown {
		t.Errorf("PgDn mapped to %v", got)
	}
}

// The `e` key must reach the model as KeyExec: without the mapping the whole
// exec path is unreachable from the keyboard.
func TestKeyEMapsToExec(t *testing.T) {
	if got := mapKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}}); got != KeyExec {
		t.Errorf("mapKey(e) = %v, want KeyExec", got)
	}
}

// Pressing e at services level must hand back a Cmd (the suspended shell) and
// leave the model disarmed, so the shell is launched once and by the TUI layer —
// never by the pure model.
func TestUpdateReturnsExecCmdAndDisarms(t *testing.T) {
	restore := stubSpaceExec(func(string, string, string) error { return nil })
	defer restore()

	m := &Model{Spaces: []Space{{Name: "dd", LimitedInfo: true, Services: []Service{
		{Name: "core", State: StateReady, Container: "sklp-deploy-dd-core"},
	}}}}
	m.Update(KeyEnter)

	tm := &teaModel{m: m, r: NewLipglossRenderer()}
	_, cmd := tm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	if cmd == nil {
		t.Fatal("e must return a Cmd that suspends the TUI and runs the shell")
	}
	if m.PendingExec != nil {
		t.Error("the TUI layer must consume the request, leaving nothing armed")
	}
}

// Opening a shell must stop the log stream first: the cockpit is about to release
// the terminal, and a stream still writing would land in the shell.
func TestExecClosesLogsPanel(t *testing.T) {
	restore := stubSpaceExec(func(string, string, string) error { return nil })
	defer restore()

	m := &Model{Spaces: []Space{{Name: "dd", LimitedInfo: true, Services: []Service{
		{Name: "core", State: StateReady, Container: "sklp-deploy-dd-core"},
	}}}}
	m.Update(KeyEnter)
	m.PendingExec = &ExecRequest{Space: "dd", Container: "sklp-deploy-dd-core", Label: "core"}
	m.LogsOpen, m.LogsLines = true, []string{"old line"}

	tm := &teaModel{m: m, r: NewLipglossRenderer()}
	tm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	if m.LogsOpen {
		t.Error("the logs panel must be closed before the terminal is handed over")
	}
}
