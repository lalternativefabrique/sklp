package cockpit

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func runes(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func openPanel(lines ...string) *teaModel {
	m := &Model{Width: 200, Height: 20, LogsOpen: true, LogsScroll: -1, SearchMatch: -1}
	for _, l := range lines {
		m.AppendLogLine(l)
	}
	return &teaModel{m: m, r: NewPlainRenderer()}
}

// While a query is being composed, a printable key is TEXT: typing "quit" must
// not quit, and "cg" must not copy or jump to the top.
func TestSearchTypingSwallowsActionKeys(t *testing.T) {
	tm := openPanel("nothing to quit here")
	tm.Update(runes("/"))
	if !tm.m.SearchTyping {
		t.Fatal("/ must open the search prompt")
	}
	for _, r := range "quit cg" {
		if _, cmd := tm.Update(runes(string(r))); cmd != nil {
			t.Fatalf("key %q produced a command while typing", r)
		}
	}
	if tm.m.SearchQuery != "quit cg" {
		t.Fatalf("SearchQuery = %q, want %q", tm.m.SearchQuery, "quit cg")
	}
	if !tm.m.LogsOpen {
		t.Fatal("typing must not have closed the panel")
	}
}

func TestSearchTypingHandlesSpaceAndBackspace(t *testing.T) {
	tm := openPanel("a b")
	tm.Update(runes("/"))
	tm.Update(runes("a"))
	tm.Update(tea.KeyMsg{Type: tea.KeySpace})
	tm.Update(runes("b"))
	if tm.m.SearchQuery != "a b" {
		t.Fatalf("SearchQuery = %q, want %q", tm.m.SearchQuery, "a b")
	}
	tm.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if tm.m.SearchQuery != "a " {
		t.Fatalf("SearchQuery = %q, want %q", tm.m.SearchQuery, "a ")
	}
}

func TestEnterCommitsAndNStepsMatches(t *testing.T) {
	tm := openPanel("hit one", "miss", "hit two")
	tm.Update(runes("/"))
	for _, r := range "hit" {
		tm.Update(runes(string(r)))
	}
	tm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if tm.m.SearchTyping {
		t.Fatal("Enter must commit the prompt")
	}
	first := tm.m.FocusedLine()
	tm.Update(runes("n"))
	if tm.m.FocusedLine() == first {
		t.Fatal("n must step to the next match inside the panel")
	}
	tm.Update(runes("N"))
	if got := tm.m.FocusedLine(); got != first {
		t.Fatalf("N returned to line %d, want %d", got, first)
	}
}

// Outside the panel n stays the cancel key of a pending confirm — turning it
// into a search verb everywhere would break the destructive prompts.
func TestNCancelsAConfirmOutsideThePanel(t *testing.T) {
	m := &Model{Spaces: []Space{{Name: "sklp-dev-1", LimitedInfo: true}}}
	tm := &teaModel{m: m, r: NewPlainRenderer()}
	m.PendingKill = "sklp-dev-1"
	tm.Update(runes("n"))
	if m.PendingKill != "" {
		t.Fatal("n must cancel a pending kill outside the logs panel")
	}
}

func TestQuitStillWorksOnceTheSearchIsCommitted(t *testing.T) {
	tm := openPanel("hit")
	tm.Update(runes("/"))
	tm.Update(runes("h"))
	tm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if _, cmd := tm.Update(runes("q")); cmd == nil {
		t.Fatal("q must quit once the prompt is closed")
	}
}
