// Package cockpit is the `sklp cockpit` full-screen read-first monitor. Its
// logic (model/view/state) is decoupled from any TUI library via the ports
// declared here: ONLY tui_bubbletea.go imports bubbletea/lipgloss, so the
// renderer can be swapped (e.g. tcell) without touching the cockpit logic.
package cockpit

import "context"

// State is our own, library-agnostic readiness enum for a glyph.
type State int

const (
	StateNone  State = iota // unknown / not applicable
	StateReady              // up and healthy
	StateBusy               // starting / reconciling
	StateIdle               // present but not active
	StateError              // failed / errored
)

// Style is our own, library-agnostic text style. The Renderer maps it to
// whatever the concrete TUI library understands (lipgloss colors, ANSI, …).
type Style int

const (
	StylePlain Style = iota
	StyleTitle
	StyleMuted
	StyleOK
	StyleWarn
	StyleErr
	StyleMatch // the focused search hit: must stand out over already-coloured logs
)

// Key is our own, library-agnostic key event. The App maps native key events
// (tea.KeyMsg, tcell.EventKey, …) onto these before calling Model.Update.
type Key int

const (
	KeyNone Key = iota
	KeyUp
	KeyDown
	KeyTab
	KeyEnter
	KeyFilter
	KeySort
	KeyLogs
	KeyVMLogs // v: the guest agent's own log, whatever is selected
	KeyQuit
	KeyBack     // Esc: back one level (services→spaces); quit only at spaces level
	KeyKill     // d: delete the selected NON-supervised space (confirm required)
	KeyPurge    // D: delete every idle space at once (confirm required)
	KeyStop     // s: stop the selected space, keeping its volumes
	KeyPageUp   // PgUp: scroll the logs one window up
	KeyPageDown // PgDn: scroll the logs one window down
	KeyHome     // g: jump to the oldest buffered log line
	KeyEnd      // G: jump back to the tail (follow mode)
	KeyConfirm  // y: confirm a pending destructive action
	KeyCopy     // c: copy the log buffer to the clipboard
	KeyExec     // e: open a shell in the selected container
	KeySearch   // /: start a search in the logs panel
	KeyNext     // n: jump to the next search match
	KeyPrev     // N: jump to the previous search match
	KeyRestart  // r: relaunch the selected stopped dev space
	KeyJump     // Ctrl+K: open the jump palette (fuzzy-find a space/container)
	KeyContext  // C: reopen the context picker to point the cockpit at another node
	KeyVMProcs  // V: expand/collapse the VM panel's process list
)

// Column describes one table column: its header and how wide it may be. Width
// is the exact rendered width; view.go computes it from the terminal size, so a
// bordered block is never truncated after the fact (which shears its border).
type Column struct {
	Header string
	Width  int
	// Flex marks a column whose Width is a computed share of the leftover space
	// rather than a fixed budget; its cells are ellipsised to fit.
	Flex bool
}

// Row is one table line plus the semantics the renderer needs to style it as a
// whole: selection background and error foreground apply to the FULL row width,
// not to a single marker cell.
type Row struct {
	Cells    []string
	Selected bool
	State    State
}

// Renderer is the component-level drawing port. It speaks in panels and rows,
// not raw cells, so view.go composes a screen without knowing the TUI library.
// Every method that draws a framed or full-width block takes an explicit width:
// the caller owns the layout, the renderer never guesses nor reflows.
type Renderer interface {
	Box(title string, body string, width int) string
	Table(cols []Column, rows []Row, width int) string
	StatusGlyph(state State) string
	Text(s string, style Style) string
	HRule(width int) string
	// Header draws the top context block: label/value pairs on the left, key
	// hints in the middle, and the product mark on the right when width allows.
	Header(fields [][2]string, keys [][2]string, width int) string
	// Crumb draws the navigation bar naming the current resource and its counts.
	Crumb(path string, right string, width int) string
	// SideBySide places two multi-line blocks horizontally adjacent. Both blocks
	// MUST already be rendered at their target width; this only joins them.
	SideBySide(left string, right string, leftWidth int) string
}

// App hides the event loop. Run reads native events, maps them to Key, calls
// Model.Update, re-renders via view.Render(m, r), and ticks a periodic refresh
// that re-reads state. It returns when the user quits or ctx is cancelled.
type App interface {
	Run(ctx context.Context, m *Model, r Renderer) error
}
