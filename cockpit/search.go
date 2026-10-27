package cockpit

import "strings"

// Search over the logs buffer. Matching is done on the UNWRAPPED buffer lines
// (the same units [c] copies), then translated to the wrapped screen rows the
// panel scrolls in — a match on a long line must scroll to the row that actually
// shows it, not to the row where the line began.

// StartSearch opens the query prompt. Only inside the logs panel: the buffer is
// what a search runs over, and the cockpit column has no text to look through.
func (m *Model) StartSearch() bool {
	if !m.LogsOpen {
		return false
	}
	m.SearchTyping = true
	m.LogsNote = ""
	return true
}

// SearchInput appends one typed rune to the query and re-focuses the first match
// at or after the current view, so the panel tracks the query as it is typed.
func (m *Model) SearchInput(r rune) bool {
	if !m.SearchTyping {
		return false
	}
	m.SearchQuery += string(r)
	m.focusFirstMatch()
	return true
}

// SearchBackspace deletes the last rune of the query, closing the prompt when it
// empties: an empty query is not a search, and leaving the prompt armed on
// nothing traps the panel's normal bindings.
func (m *Model) SearchBackspace() bool {
	if !m.SearchTyping {
		return false
	}
	if m.SearchQuery == "" {
		m.SearchTyping = false
		return true
	}
	r := []rune(m.SearchQuery)
	m.SearchQuery = string(r[:len(r)-1])
	m.focusFirstMatch()
	return true
}

// CommitSearch closes the prompt, keeping the query active for n/N. An empty
// query clears the search instead of committing nothing.
func (m *Model) CommitSearch() bool {
	if !m.SearchTyping {
		return false
	}
	m.SearchTyping = false
	if m.SearchQuery == "" {
		return m.ClearSearch()
	}
	if len(m.searchMatches()) == 0 {
		m.LogsNote = "no match for " + m.SearchQuery
	}
	return true
}

// ClearSearch drops the query and the prompt, leaving the scroll position alone
// so Esc does not also teleport the reader back to the tail.
func (m *Model) ClearSearch() bool {
	if m.SearchQuery == "" && !m.SearchTyping {
		return false
	}
	m.SearchTyping = false
	m.SearchQuery = ""
	m.SearchMatch = -1
	m.LogsNote = ""
	return true
}

// searchMatches lists the BUFFER line indices holding the query, in order.
func (m *Model) searchMatches() []int {
	if m.SearchQuery == "" {
		return nil
	}
	needle := strings.ToLower(m.SearchQuery)
	var out []int
	for i, l := range m.LogsLines {
		if strings.Contains(strings.ToLower(l), needle) {
			out = append(out, i)
		}
	}
	return out
}

// NextMatch focuses the following hit, wrapping past the last one back to the
// first: a log buffer is a ring the reader circles, not a list they fall off.
func (m *Model) NextMatch() bool { return m.stepMatch(+1) }

// PrevMatch focuses the preceding hit, wrapping the same way.
func (m *Model) PrevMatch() bool { return m.stepMatch(-1) }

func (m *Model) stepMatch(delta int) bool {
	hits := m.searchMatches()
	if len(hits) == 0 {
		if m.SearchQuery != "" {
			m.LogsNote = "no match for " + m.SearchQuery
		}
		return false
	}
	next := m.SearchMatch + delta
	if m.SearchMatch < 0 {
		// No hit focused yet (fresh query, or the previous one scrolled out of the
		// buffer): step from where the reader is looking, not from the top.
		next = m.matchNearestToView(delta)
	}
	next = ((next % len(hits)) + len(hits)) % len(hits)
	m.SearchMatch = next
	m.scrollToLine(hits[next])
	m.LogsNote = matchLabel(next, len(hits), m.SearchQuery)
	return true
}

// matchNearestToView picks the hit a first n/N should land on: the first one at
// or after the top visible row going forward, the last one before it going back.
func (m *Model) matchNearestToView(delta int) int {
	hits := m.searchMatches()
	top := m.topBufferLine()
	if delta < 0 {
		for i := len(hits) - 1; i >= 0; i-- {
			if hits[i] < top {
				return i
			}
		}
		return len(hits) - 1
	}
	for i, h := range hits {
		if h >= top {
			return i
		}
	}
	return 0
}

// matchLabel is the panel's search status: which hit of how many.
func matchLabel(i, n int, query string) string {
	return itoa(i+1) + "/" + itoa(n) + " match(es) for " + query
}

// focusFirstMatch re-points the focus while the query is being typed, without
// consuming a hit position the operator has not asked for yet.
func (m *Model) focusFirstMatch() {
	hits := m.searchMatches()
	if len(hits) == 0 {
		m.SearchMatch = -1
		m.LogsNote = ""
		return
	}
	m.SearchMatch = m.matchNearestToView(+1)
	m.scrollToLine(hits[m.SearchMatch])
	m.LogsNote = matchLabel(m.SearchMatch, len(hits), m.SearchQuery)
}

// FocusedLine is the buffer index of the focused match, -1 when none. The view
// highlights that row so the hit is visible among the lines around it.
func (m *Model) FocusedLine() int {
	hits := m.searchMatches()
	if m.SearchMatch < 0 || m.SearchMatch >= len(hits) {
		return -1
	}
	return hits[m.SearchMatch]
}

// scrollToLine pins the view so buffer line i is visible, centred when the
// buffer allows it. It converts to SCREEN rows first: the panel scrolls in
// wrapped rows, so a buffer index is not a scroll position.
func (m *Model) scrollToLine(i int) {
	row := m.bufferLineToRow(i)
	height := logsHeight(m.Height, logsPaneExtraRows(m))
	top := row - height/2
	if top < 0 {
		top = 0
	}
	maxTop := len(m.logRows()) - height
	if maxTop < 0 {
		maxTop = 0
	}
	if top >= maxTop {
		// The hit sits in the tail window; follow mode shows it and keeps new
		// lines arriving, which is what a reader at the bottom wants.
		m.LogsScroll = -1
		return
	}
	m.LogsScroll = top
}

// bufferLineToRow maps a buffer index onto the first screen row that line
// occupies once wrapped.
func (m *Model) bufferLineToRow(i int) int {
	width := logsTextWidth(m)
	row := 0
	for n, l := range m.LogsLines {
		if n == i {
			return row
		}
		row += rowsFor(l, width)
	}
	return row
}

// topBufferLine is the buffer index of the topmost visible row, so a search can
// start from what the reader is actually looking at.
func (m *Model) topBufferLine() int {
	rows := m.logRows()
	top := m.LogsScroll
	if top < 0 {
		height := logsHeight(m.Height, logsPaneExtraRows(m))
		top = len(rows) - height
	}
	if top <= 0 {
		return 0
	}
	width := logsTextWidth(m)
	acc := 0
	for i, l := range m.LogsLines {
		acc += rowsFor(l, width)
		if acc > top {
			return i
		}
	}
	return len(m.LogsLines) - 1
}

// rowsFor is how many screen rows a buffer line costs once hard-wrapped.
func rowsFor(l string, width int) int {
	if width < 1 {
		return 1
	}
	n := len([]rune(l))
	if n == 0 {
		return 1
	}
	return (n + width - 1) / width
}
