package cockpit

import "testing"

// searchModel is a panel opened on a buffer wide enough that no line wraps, so
// buffer indices and screen rows coincide and a scroll assertion is readable.
func searchModel(t *testing.T, lines ...string) *Model {
	t.Helper()
	m := &Model{Width: 200, Height: 20, LogsOpen: true, LogsScroll: -1, SearchMatch: -1}
	for _, l := range lines {
		m.AppendLogLine(l)
	}
	return m
}

func typeQuery(m *Model, q string) {
	m.StartSearch()
	for _, r := range q {
		m.SearchInput(r)
	}
	m.CommitSearch()
}

func TestSearchFindsMatchCaseInsensitively(t *testing.T) {
	m := searchModel(t, "boot", "ERROR: boom", "done")
	typeQuery(m, "error")
	if got := m.FocusedLine(); got != 1 {
		t.Fatalf("FocusedLine = %d, want 1", got)
	}
	if m.SearchTyping {
		t.Fatal("commit should close the prompt")
	}
}

func TestSearchNoMatchReportsHonestly(t *testing.T) {
	m := searchModel(t, "boot", "done")
	typeQuery(m, "nope")
	if got := m.FocusedLine(); got != -1 {
		t.Fatalf("FocusedLine = %d, want -1", got)
	}
	if m.LogsNote == "" {
		t.Fatal("a query matching nothing must say so")
	}
}

func TestSearchNextWrapsAround(t *testing.T) {
	m := searchModel(t, "hit a", "miss", "hit b")
	typeQuery(m, "hit")
	first := m.FocusedLine()
	m.NextMatch()
	second := m.FocusedLine()
	if first == second {
		t.Fatalf("NextMatch did not move: still line %d", first)
	}
	m.NextMatch()
	if got := m.FocusedLine(); got != first {
		t.Fatalf("NextMatch did not wrap: line %d, want %d", got, first)
	}
}

func TestSearchPrevStepsBackwards(t *testing.T) {
	m := searchModel(t, "hit a", "miss", "hit b")
	typeQuery(m, "hit")
	m.NextMatch()
	after := m.FocusedLine()
	m.PrevMatch()
	if got := m.FocusedLine(); got == after {
		t.Fatalf("PrevMatch did not move: still line %d", got)
	}
}

func TestSearchScrollsAMatchOutOfTheTailIntoView(t *testing.T) {
	lines := make([]string, 0, 200)
	lines = append(lines, "needle at the very top")
	for i := 0; i < 199; i++ {
		lines = append(lines, "filler")
	}
	m := searchModel(t, lines...)
	typeQuery(m, "needle")
	if m.LogsScroll < 0 {
		t.Fatal("a match far above the tail must leave follow mode")
	}
	height := logsHeight(m.Height, logsPaneExtraRows(m))
	if m.LogsScroll > 0 || height <= 0 {
		t.Fatalf("LogsScroll = %d, want the top window (0)", m.LogsScroll)
	}
}

func TestSearchBackspaceClosesPromptWhenEmptied(t *testing.T) {
	m := searchModel(t, "a")
	m.StartSearch()
	m.SearchInput('x')
	m.SearchBackspace()
	if m.SearchQuery != "" {
		t.Fatalf("SearchQuery = %q, want empty", m.SearchQuery)
	}
	m.SearchBackspace()
	if m.SearchTyping {
		t.Fatal("backspacing past an empty query must close the prompt")
	}
}

func TestClearSearchKeepsScrollPosition(t *testing.T) {
	m := searchModel(t, "needle", "a", "b")
	typeQuery(m, "needle")
	before := m.LogsScroll
	m.ClearSearch()
	if m.SearchQuery != "" || m.FocusedLine() != -1 {
		t.Fatal("ClearSearch must drop the query and its focus")
	}
	if m.LogsScroll != before {
		t.Fatalf("LogsScroll = %d, want it left at %d", m.LogsScroll, before)
	}
}

func TestSearchOnlyInsideTheLogsPanel(t *testing.T) {
	m := &Model{Width: 200, Height: 20}
	if m.StartSearch() {
		t.Fatal("search must not open outside the logs panel")
	}
}

func TestCloseLogsClearsSearch(t *testing.T) {
	m := searchModel(t, "needle")
	typeQuery(m, "needle")
	m.CloseLogs()
	if m.SearchQuery != "" || m.SearchTyping || m.SearchMatch != -1 {
		t.Fatal("closing the panel must clear the search state")
	}
}

func TestSearchKeysRouteThroughUpdate(t *testing.T) {
	m := searchModel(t, "hit a", "miss", "hit b")
	if !m.Update(KeySearch) || !m.SearchTyping {
		t.Fatal("KeySearch must open the prompt")
	}
	m.SearchInput('h')
	m.SearchInput('i')
	m.SearchInput('t')
	if !m.Update(KeyEnter) || m.SearchTyping {
		t.Fatal("Enter must commit the prompt")
	}
	first := m.FocusedLine()
	m.Update(KeyNext)
	if m.FocusedLine() == first {
		t.Fatal("KeyNext must step to the following match")
	}
	m.Update(KeyPrev)
	if got := m.FocusedLine(); got != first {
		t.Fatalf("KeyPrev returned to line %d, want %d", got, first)
	}
}

func TestEscapeClearsSearchBeforeClosingThePanel(t *testing.T) {
	m := searchModel(t, "needle")
	typeQuery(m, "needle")
	if !m.Update(KeyBack) {
		t.Fatal("Esc with an active search must clear it")
	}
	if m.SearchQuery != "" {
		t.Fatal("Esc did not clear the query")
	}
	if !m.LogsOpen {
		t.Fatal("Esc must not close the panel while a search was active")
	}
}

func TestWrappedMatchScrollsToTheRowThatShowsIt(t *testing.T) {
	// A pane narrow enough that the needle falls past the first wrap: scrolling
	// to the line's FIRST row would leave the match itself off-screen.
	m := &Model{Width: 60, Height: 12, LogsOpen: true, LogsScroll: -1, SearchMatch: -1}
	for i := 0; i < 40; i++ {
		m.AppendLogLine("filler line")
	}
	long := ""
	for i := 0; i < 20; i++ {
		long += "0123456789"
	}
	m.AppendLogLine(long + "needle")
	typeQuery(m, "needle")
	if m.FocusedLine() != len(m.LogsLines)-1 {
		t.Fatalf("FocusedLine = %d, want the last line", m.FocusedLine())
	}
	rows := m.logRows()
	if len(rows) <= len(m.LogsLines) {
		t.Fatal("expected the long line to wrap into several rows")
	}
}
