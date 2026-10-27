package cockpit

import (
	"fmt"
	"path"
	"strings"
)

// fallbackWidth is used when the terminal size is unknown (non-TTY snapshot).
const fallbackWidth = 80

// splitGutter is the column gap between the cockpit and the logs panel.
const splitGutter = 2

// logsMinWidth is the narrowest logs pane worth splitting for. Log lines carry
// long identifiers (space hostnames, request paths, UUIDs); under this the pane
// wraps every line into unreadable fragments, so the logs take over the screen.
const logsMinWidth = 60

// cockpitMinWidth is the narrowest the cockpit column may be squeezed to before
// the split is abandoned in favour of a full-screen logs pane.
const cockpitMinWidth = 44

// splitMinWidth is the terminal width below which the logs panel takes over the
// whole screen instead of splitting.
const splitMinWidth = cockpitMinWidth + logsMinWidth + splitGutter

// Render composes the whole cockpit screen from the Model using ONLY the
// Renderer port — it never imports a TUI library. With the logs panel open it
// splits side-by-side on a wide terminal and takes over the screen on a narrow
// one; every block is rendered AT its target width, never clipped afterwards.
func Render(m *Model, r Renderer) string {
	width := termWidth(m.Width)
	if m.ContextsOpen {
		return renderContexts(m, r, width)
	}
	if !m.LogsOpen {
		return renderCockpit(m, r, width)
	}
	if width < splitMinWidth {
		return renderLogsPane(m, r, width)
	}
	left := cockpitWidth(width)
	return r.SideBySide(renderCockpit(m, r, left), renderLogsPane(m, r, logsPaneWidth(m)), left)
}

// logsPaneWidth is the width the logs pane is rendered at: the whole screen on a
// narrow terminal, the split's right column otherwise. Scrolling reads it too, so
// it wraps on the same width the panel paints.
func logsPaneWidth(m *Model) int {
	width := termWidth(m.Width)
	if width < splitMinWidth {
		return width
	}
	return width - cockpitWidth(width) - splitGutter
}

func termWidth(w int) int {
	if w <= 0 {
		return fallbackWidth
	}
	return w
}

// cockpitWidth splits the screen when the logs panel is open. The logs pane gets
// the larger share: build-log lines (package paths, docker steps) are long,
// while the spaces table compresses gracefully. The logs pane keeps a hard floor
// of logsMinWidth — the cockpit column yields first, since a starved logs pane
// wraps every line into fragments while the spaces table just drops columns.
func cockpitWidth(width int) int {
	avail := width - splitGutter
	left := avail * 45 / 100
	if max := avail - logsMinWidth; left > max {
		left = max
	}
	if left < cockpitMinWidth {
		left = cockpitMinWidth
	}
	return left
}

// renderCockpit draws the cockpit column: context header, breadcrumb, the rows
// table, then any transient prompt and the footer. It never references logs.
func renderCockpit(m *Model, r Renderer, width int) string {
	var b strings.Builder

	b.WriteString(r.Header(headerFields(m, r), headerKeys(m), width))
	b.WriteString("\n")
	b.WriteString(r.Crumb(crumbPath(m), crumbRight(m), width))
	b.WriteString("\n\n")

	if m.Err != "" {
		b.WriteString(r.Box("Error", note(r, m.Err, StyleErr, width-boxChrome), width))
		return b.String()
	}

	if m.JumpOpen {
		b.WriteString(renderJump(m, r, width))
		b.WriteString("\n\n")
	}

	if len(m.Spaces) == 0 && len(m.HostRuns) == 0 {
		b.WriteString(note(r, "no spaces on this host — `sklp space up` or `sklp deploy` to bring one up", StyleMuted, width))
	} else {
		b.WriteString(renderSpaceList(m, r, width))
		if detail := renderDetail(m, r, width); detail != "" {
			b.WriteString("\n\n")
			b.WriteString(detail)
		}
	}
	if vm := renderVM(m, r, width); vm != "" {
		b.WriteString("\n\n")
		b.WriteString(vm)
	}
	if hint := m.FilterHint(); hint != "" {
		b.WriteString("\n\n")
		b.WriteString(note(r, hint, StyleWarn, width))
	}
	b.WriteString("\n\n")
	b.WriteString(renderStatusLine(m, r, width))
	return b.String()
}

// jumpResultsCap bounds how many hits the palette lists at once: past a
// handful the operator has already narrowed the query further, and a long
// list would push the footer off a small terminal.
const jumpResultsCap = 8

// renderJump draws the Ctrl+K palette: the typed query, then its matches
// (space, or "space › container" for a container hit) with the highlighted
// one marked — the same table styling as the spaces list, so the palette
// reads as part of the same UI rather than a foreign overlay.
func renderJump(m *Model, r Renderer, width int) string {
	hits := m.jumpHits()
	title := "jump: " + m.JumpQuery + "▏"
	if m.JumpQuery == "" {
		return r.Box(title, r.Text("type a space or container name…", StyleMuted), width)
	}
	if len(hits) == 0 {
		return r.Box(title, r.Text("no match for "+m.JumpQuery, StyleWarn), width)
	}
	shown := hits
	truncated := 0
	if len(shown) > jumpResultsCap {
		truncated = len(shown) - jumpResultsCap
		shown = shown[:jumpResultsCap]
	}
	rows := make([]Row, 0, len(shown))
	for i, h := range shown {
		cells := []string{h.label, h.node}
		rows = append(rows, Row{Cells: cells, Selected: i == m.JumpIndex})
	}
	cols := []Column{
		{Header: "MATCH", Flex: true},
		{Header: "NODE", Flex: true},
	}
	body := r.Table(distribute(cols, width-boxChrome, rows), rows, width-boxChrome)
	if truncated > 0 {
		body += "\n" + note(r, "+"+itoa(truncated)+" more — narrow the query", StyleMuted, width-boxChrome)
	}
	return r.Box(title, body, width)
}

// renderContexts draws the context picker: the first screen of the cockpit, and
// the overlay [C] reopens. It replaces the whole view rather than floating over
// it — before the first choice there is no inventory behind it to show.
func renderContexts(m *Model, r Renderer, width int) string {
	rows := make([]Row, 0, len(m.ContextChoices))
	for i, c := range m.ContextChoices {
		target := c.Target
		if c.Name == LocalContext {
			target = "this machine"
		}
		active := ""
		if m.ContextChosen && c.Name == m.Context {
			active = "active"
		}
		rows = append(rows, Row{
			Cells:    []string{c.Label(), target, active},
			Selected: i == m.ContextIndex,
		})
	}
	cols := []Column{
		{Header: "CONTEXT", Flex: true},
		{Header: "TARGET", Flex: true},
		{Header: "", Width: 6},
	}
	body := r.Table(distribute(cols, width-boxChrome, rows), rows, width-boxChrome)
	hint := "↑/↓ move · enter select · q quit"
	if m.ContextChosen {
		hint = "↑/↓ move · enter select · esc cancel · q quit"
	}
	body += "\n" + note(r, hint, StyleMuted, width-boxChrome)
	return r.Box("select context", body, width)
}

// footerAbbrevWidth is the column count below which the footer drops its verbs
// and keeps only the keys: wrapping it mid-word ("[d] de / lete") is less
// readable than the compact form.
const footerAbbrevWidth = 78

// noteIndent is the two-column gutter every free-standing cockpit line carries,
// aligning it with the tables' tableIndent.
const noteIndent = 2

// boxChrome is the horizontal cost of a framed block's border and padding.
const boxChrome = logsBoxChrome

// note renders a free-standing cockpit line at an exact width. Without this the
// line escapes the column it was rendered for, and SideBySide aligns the whole
// right-hand pane on the overflow — which is how one long volume name squeezed
// the logs pane down to a few columns.
func note(r Renderer, s string, style Style, width int) string {
	avail := width - noteIndent
	if avail < 1 {
		return r.Text(strings.Repeat(" ", noteIndent)+s, style)
	}
	rows := wrapLines(strings.Split(s, "\n"), avail)
	for i, row := range rows {
		rows[i] = r.Text(strings.Repeat(" ", noteIndent)+row, style)
	}
	return strings.Join(rows, "\n")
}

// renderStatusLine carries the one transient message that matters right now: a
// pending destructive confirm outranks a note, which outranks the key hints.
func renderStatusLine(m *Model, r Renderer, width int) string {
	switch {
	case m.PendingKill != "":
		return note(r, "delete space "+m.PendingKill+killVolumeWarning(m)+"? [↵/y] confirm  [n] cancel", StyleErr, width)
	case m.PendingPurge:
		return note(r, fmt.Sprintf("purge idle spaces (at least %d of %d) and their %d volume(s)? [↵/y] confirm  [n] cancel",
			m.idleSpaceCount(), len(m.Spaces), m.idleVolumeCount()), StyleErr, width)
	case m.JumpOpen:
		return note(r, "[↑/↓] pick  [↵] jump  [esc] cancel", StyleMuted, width)
	case m.Note != "":
		return note(r, m.Note, StyleWarn, width)
	}
	return note(r, cockpitFooter(m, width), StyleMuted, width)
}

// headerFields is the left column of the header: the context the operator needs
// to know what they are looking at.
func headerFields(m *Model, r Renderer) [][2]string {
	h := m.Host
	fields := [][2]string{
		// The context leads the header: every count below describes THAT node,
		// and mistaking prod for local is the costly confusion here.
		{"Context:", m.ContextLabel()},
		{"Spaces:", fmt.Sprintf("%d · %d supervised", h.Spaces, h.Supervised)},
		{"Services:", fmt.Sprintf("%d · %d ready %d busy %d err", h.Services, h.Ready, h.Busy, h.Errored)},
	}
	if f := m.Flow; f.Active() {
		flow := f.Branch + " vs " + f.Onto
		if f.Changed > 0 {
			flow += fmt.Sprintf("  Δ%d file(s)", f.Changed)
		}
		fields = append(fields, [2]string{"Flow:", flow})
		fields = append(fields, [2]string{"", flowMilestones(m, r)})
	} else {
		fields = append(fields, [2]string{"Flow:", "no active feature flow — `sklp flow start <name>` to begin"})
	}
	if sp := m.Selection(); sp != nil {
		fields = append(fields, [2]string{"Space:", sp.Name})
		if app := spaceApp(*sp); app != "—" {
			fields = append(fields, [2]string{"App:", app})
		}
	}
	return fields
}

// headerKeys is the middle column: the bindings that apply at this level.
func headerKeys(m *Model) [][2]string {
	if m.Level == LevelServices {
		return [][2]string{
			{"<↑↓>", "pick service"},
			{"<e>", "shell"},
			{"<l>", "logs"},
			{"<esc>", "back"},
			{"<v>", "vm logs"},
			{"<ctrl+k>", "jump"},
			{"<C>", "context"},
			{"<q>", "quit"},
		}
	}
	return [][2]string{
		{"<↑↓>", "navigate"},
		{"<ctrl+k>", "jump to space/container"},
		{"<↵>", "drill in"},
		{"<l>", "logs"},
		{"<r>", "relaunch dev"},
		{"<s>", "stop (keeps data)"},
		{"<d/D>", "delete / purge"},
		{"<v>", "vm logs"},
		{"<V>", "vm procs"},
		{"<C>", "context"},
		{"<q>", "quit"},
	}
}

func crumbPath(m *Model) string {
	if m.Level == LevelServices {
		if sp := m.Selection(); sp != nil {
			return fmt.Sprintf("spaces > %s > services(%d)", sp.Name, len(sp.Services))
		}
		return "services"
	}
	return fmt.Sprintf("spaces(%d)", m.rowCount())
}

func crumbRight(m *Model) string {
	return fmt.Sprintf("filter:%s  sort:%s", m.Filter, m.Sort)
}

// cockpitFooter returns the keybinding hint for the current drill-in level,
// dropping the verbs when the column is too narrow to hold them on one line.
func cockpitFooter(m *Model, width int) string {
	if m.Level == LevelServices {
		return "[↑/↓] pick svc  [e] shell  [l] logs  [esc] back  [ctrl+k] jump"
	}
	if width < footerAbbrevWidth {
		// Wrapping picks its own break point, mid-word; breaking it here keeps every
		// key paired with the verb that explains it.
		return "[↑/↓] select  [↵] enter  [l] logs  [r] relaunch  [v] vm  [q] quit\n[f] filter  [o] order  [s] stop  [d] delete  [D] purge idle  [ctrl+k] jump"
	}
	return "[↑/↓] select  [↵] enter  [f] filter  [o] order  [l] logs  [r] relaunch  [v] vm  [s] stop  [d] delete  [D] purge idle  [ctrl+k] jump  [q] quit"
}

// renderLogsPane draws the logs column: the panel plus its own footer hint, so
// the split's right column is self-contained.
func renderLogsPane(m *Model, r Renderer, width int) string {
	var b strings.Builder
	b.WriteString(renderLogsPanel(m, r, width))
	b.WriteString("\n")
	if prompt := searchPrompt(m); prompt != "" {
		b.WriteString(note(r, prompt, StyleWarn, width))
		b.WriteString("\n")
	}
	if m.LogsNote != "" {
		b.WriteString(note(r, m.LogsNote, StyleWarn, width))
		b.WriteString("\n")
	}
	b.WriteString(note(r, logsFooter(width), StyleMuted, width))
	return b.String()
}

// logsFooter lists the panel bindings, breaking between them rather than letting
// the wrapper split one mid-word on a narrow pane.
func logsFooter(width int) string {
	const full = "[↑/↓] scroll  [PgUp/PgDn] page  [g/G] top/tail  [/] search  [n/N] match  [c] copy  [l/esc] close"
	if width-noteIndent < len([]rune(full)) {
		return "[↑/↓] scroll  [PgUp/PgDn] page  [g/G] top/tail\n[/] search  [n/N] match  [c] copy  [l/esc] close"
	}
	return full
}

// searchPrompt is the search line under the log box: the live query while it is
// being typed, or a reminder of the committed one so the reader knows what n/N
// is stepping through. "" when no search is active.
func searchPrompt(m *Model) string {
	if m.SearchTyping {
		return "/" + m.SearchQuery + "▏"
	}
	if m.SearchQuery != "" {
		return "/" + m.SearchQuery + "  [n/N] next/prev  [esc] clear"
	}
	return ""
}

// logsPanelHeight is the fallback log-window height when the terminal size is
// unknown. When the height IS known the panel grows to fill it (logsHeightFor).
const logsPanelHeight = 20

// logsChrome is the number of terminal rows the logs pane spends on non-log
// content: the box's top+bottom border, the title line, and the footer hint
// (plus its blank separator). Subtracted from the terminal height so the log
// window fills the rest of the screen without pushing past it.
const logsChrome = 6

// logsHeightFor returns how many log rows to show for a terminal of the given
// height: the screen minus the pane chrome, floored at a small minimum so a
// tiny terminal still shows something. Falls back to logsPanelHeight when the
// height is unknown (0).
func logsHeightFor(termHeight int) int {
	return logsHeight(termHeight, 0)
}

// logsHeight is logsHeightFor minus the rows the pane spends below the box: a
// transient action note, and a footer that wrapped onto a second line. Counting
// them keeps the panel inside the screen instead of pushing past its bottom.
func logsHeight(termHeight, extraRows int) int {
	if termHeight <= 0 {
		return logsPanelHeight
	}
	h := termHeight - logsChrome - extraRows
	if h < 5 {
		return 5
	}
	return h
}

// logsPaneExtraRows counts the rows below the log box: the action note when set,
// plus any line the footer wrapped onto.
func logsPaneExtraRows(m *Model) int {
	n := 0
	if searchPrompt(m) != "" {
		n++
	}
	if m.LogsNote != "" {
		n++
	}
	n += strings.Count(logsFooter(logsPaneWidth(m)), "\n")
	return n
}

// renderLogsPanel draws the framed live-logs panel. It shows an honest message
// when logs are unavailable, otherwise the visible window of the ring buffer
// per LogsScroll (follow mode pins to the tail).
func renderLogsPanel(m *Model, r Renderer, width int) string {
	title := "logs: " + m.LogsService
	if m.LogsService == "" {
		title = "logs: " + m.LogsSpace
	}
	if m.LogsCI {
		kind := "run-log"
		if strings.HasPrefix(path.Base(m.LogsPath), "dev-") {
			kind = "dev run-log"
		} else if strings.HasPrefix(path.Base(m.LogsPath), "ci-") {
			kind = "CI run-log"
		}
		title = "logs: " + m.LogsSpace + " (" + kind + ")"
	}
	if m.LogsVM {
		title = "logs: vm (guest agent)"
	}
	if m.LogsErr != "" {
		return r.Box(title, r.Text(m.LogsErr, StyleWarn), width)
	}
	if len(m.LogsLines) == 0 {
		return r.Box(title, r.Text("streaming… (no output yet)", StyleMuted), width)
	}
	// A long log line wraps inside the box, so N buffer lines can cost far more
	// than N screen rows and the panel overflows the terminal. Wrap first, then
	// window on the resulting screen rows.
	rows := m.logRows()
	height := logsHeight(m.Height, logsPaneExtraRows(m))
	top := visibleTop(len(rows), m.LogsScroll, height)
	lines := visibleLogLines(rows, m.LogsScroll, height)
	lines = markMatch(m, r, lines, top)
	return r.Box(title+logsPosition(m, len(rows), top, height), strings.Join(lines, "\n"), width)
}

// logsPosition annotates the panel title with where the window sits in the
// buffer. Without it a paused view and a live one look identical — which on a
// service repeating the same line is indistinguishable from a frozen panel.
func logsPosition(m *Model, rows, top, height int) string {
	if m.LogsScroll < 0 {
		return "  · following"
	}
	pct := 100
	if max := rows - height; max > 0 {
		pct = top * 100 / max
	}
	return fmt.Sprintf("  · paused %d%% — [G] follow", pct)
}

// markMatch styles the focused hit's screen rows so it stands out among the
// lines around it. It works in screen rows, the units the window is expressed
// in, and a wrapped hit is marked across every row it occupies — marking only
// its first row leaves the match itself unstyled when it fell past the wrap.
func markMatch(m *Model, r Renderer, lines []string, top int) []string {
	line := m.FocusedLine()
	if line < 0 || len(lines) == 0 {
		// The gutter column is reserved in logsTextWidth whether or not a search is
		// running, so it must be painted either way — indenting only during a
		// search would shift every line sideways the moment one starts.
		out := make([]string, len(lines))
		for i, l := range lines {
			out[i] = matchGutterBlank + l
		}
		return out
	}
	start := m.bufferLineToRow(line)
	end := start + rowsFor(m.LogsLines[line], logsTextWidth(m))
	out := make([]string, len(lines))
	for i, l := range lines {
		row := top + i
		if row < start || row >= end {
			out[i] = matchGutterBlank + l
			continue
		}
		// A log line carries its OWN colors (the run-log's [svc]/INFO tints). A
		// style wrapped around them dies at their first reset, leaving only the
		// text past the last one marked — which is why a wrapped match used to
		// highlight its continuation row and nothing else. Strip them first so
		// the mark owns the whole row.
		out[i] = r.Text(matchGutterMark+stripANSI(l), StyleMatch)
	}
	return out
}

// matchGutterMark flags the focused match's rows. It survives a terminal that
// renders reverse video poorly, and it reads at a glance while scrolling — the
// colour alone is lost among logs that are already coloured.
const (
	matchGutterMark  = "▌"
	matchGutterBlank = " "
)

// stripANSI drops SGR escape sequences from s, leaving the visible text.
func stripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := i + 1
			// CSI … final byte in @-~ ends the sequence; anything else is a short
			// two-byte escape.
			if j < len(s) && s[j] == '[' {
				for j++; j < len(s) && (s[j] < '@' || s[j] > '~'); j++ {
				}
			}
			if j < len(s) {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// visibleTop is the first screen row visibleLogLines will show, so a caller can
// map its returned slice back onto absolute row numbers.
func visibleTop(n, scroll, height int) int {
	if height <= 0 || n == 0 {
		return 0
	}
	top := scroll
	if top < 0 || top > n-1 {
		top = n - height
	}
	if top < 0 {
		top = 0
	}
	return top
}

// logsBoxChrome is the horizontal cost of the panel frame: two border columns
// plus the box padding on each side.
const logsBoxChrome = 4

// logsTextWidth is the width log text is wrapped at: the pane minus the frame
// and the match gutter. Scrolling, match-to-row mapping and rendering MUST all
// read it from here — a width computed twice is a width that drifts, and the
// panel then scrolls by an amount it does not paint.
func logsTextWidth(m *Model) int {
	w := logsPaneWidth(m) - logsBoxChrome - len([]rune(matchGutterBlank))
	if w < 1 {
		return 1
	}
	return w
}

// wrapLines hard-wraps each line to width, so one slice entry is one screen row.
func wrapLines(lines []string, width int) []string {
	if width < 1 {
		return lines
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		r := []rune(l)
		if len(r) == 0 {
			out = append(out, "")
			continue
		}
		for len(r) > width {
			out = append(out, string(r[:width]))
			r = r[width:]
		}
		out = append(out, string(r))
	}
	return out
}

// visibleLogLines returns the window of lines to show. scroll<0 pins to the
// tail (follow); otherwise scroll is the top visible index.
func visibleLogLines(all []string, scroll, height int) []string {
	n := len(all)
	if height <= 0 || n == 0 {
		return nil
	}
	top := scroll
	if top < 0 || top > n-1 {
		top = n - height
	}
	if top < 0 {
		top = 0
	}
	end := top + height
	if end > n {
		end = n
	}
	return all[top:end]
}

// flowMilestones renders the lifecycle as a single glyph strip. No active flow
// → "", so the header shows the start hint instead of a fabricated lifecycle.
func flowMilestones(m *Model, r Renderer) string {
	f := m.Flow
	if !f.Active() {
		return ""
	}
	return milestoneLine(r, "start", f.Start) + "  " +
		milestoneLine(r, "coding", f.Coding) + "  " +
		milestoneLine(r, "CI", f.CI) + "  " +
		milestoneLine(r, "PR", f.PR)
}

// milestoneLine renders one lifecycle step as "<glyph> <label>". Outside a table
// nothing else owns the color, so the glyph carries its own.
func milestoneLine(r Renderer, label string, ms Milestone) string {
	st := milestoneState(ms)
	return r.Text(r.StatusGlyph(st), styleFor(st)) + " " + label
}

// styleFor maps a readiness state onto the text style that conveys it.
func styleFor(st State) Style {
	switch st {
	case StateReady:
		return StyleOK
	case StateBusy:
		return StyleWarn
	case StateError:
		return StyleErr
	default:
		return StyleMuted
	}
}

// milestoneState maps a Milestone onto a glyph State.
func milestoneState(ms Milestone) State {
	switch ms {
	case MilestoneDone:
		return StateReady
	case MilestoneActive:
		return StateBusy
	case MilestoneTodo:
		return StateIdle
	default:
		return StateNone // Unknown
	}
}

// renderSpaceList draws the spaces table with a selection marker on the active
// row and a per-space readiness glyph. Multiple nodes are shown as SEPARATE
// tables under a "NODE · <name>" heading rather than a NODE column: a node is
// the machine each row of spaces runs on, not an attribute of one row, and two
// nodes can list a same-named space — grouping them apart removes the
// ambiguity instead of asking the reader to compare a text cell. A single-node
// cockpit (the common case) renders exactly as before: one table, no heading.
// Host-only pipelines (space:false, e.g. publish) are appended in the SAME box
// under their own "publish" sub-heading (styled like the column header, not a
// row), as a further table. They share the single ↑/↓ selection so the
// operator reaches them in one list, then [l] tails their run log.
func renderSpaceList(m *Model, r Renderer, width int) string {
	withRepo := width >= repoColMinWidth
	groups := groupByNode(m.Spaces)

	var parts []string
	for _, g := range groups {
		rows := make([]Row, 0, len(g.rows))
		for _, nr := range g.rows {
			sp := nr.space
			st := spaceState(sp)
			cells := []string{r.StatusGlyph(st), sp.Name, spaceApp(sp), spaceCount(sp), spaceVolumes(sp)}
			if withRepo {
				cells = append(cells, sp.Repo)
			}
			rows = append(rows, Row{Cells: cells, Selected: nr.index == m.Selected, State: st})
		}
		cols := []Column{
			{Header: "", Width: 1},
			{Header: "SPACE", Flex: true},
			{Header: "APP", Flex: true},
			{Header: "SVC", Width: 6},
			{Header: "VOL", Width: 3},
		}
		if withRepo {
			cols = append(cols, Column{Header: "REPO", Flex: true})
		}
		if len(groups) > 1 {
			parts = append(parts, note(r, "NODE · "+g.node, StyleMuted, width))
		}
		parts = append(parts, r.Table(distribute(cols, width, rows), rows, width))
	}
	body := strings.Join(parts, "\n\n")

	if len(m.HostRuns) > 0 {
		hrRows := make([]Row, 0, len(m.HostRuns))
		for i, hr := range m.HostRuns {
			runlog := "no log"
			if hr.HasLog {
				runlog = "log"
			}
			hrRows = append(hrRows, Row{
				Cells:    []string{r.StatusGlyph(StateNone), hr.Name, runlog},
				Selected: len(m.Spaces)+i == m.Selected,
			})
		}
		hrCols := []Column{
			{Header: "", Width: 1},
			{Header: "PIPELINE", Flex: true},
			{Header: "RUN LOG", Width: 7},
		}
		body += "\n\n" + r.Table(distribute(hrCols, width, hrRows), hrRows, width)
	}
	return body
}

// nodeRow pairs a space with its index in the flat m.Spaces slice, so a group
// built by picking rows out of order (see groupByNode) can still tell
// renderSpaceList which row is m.Selected.
type nodeRow struct {
	space Space
	index int
}

// nodeGroup is every row on one node, in their original m.Spaces order.
type nodeGroup struct {
	node string
	rows []nodeRow
}

// groupByNode buckets spaces by their Node field, preserving first-seen node
// order. sortSpaces already groups by node, so each bucket is a contiguous run
// and the flat index order matches the rendered order — which is what lets
// ↑/↓ walk down the screen. Each row keeps its m.Spaces index for selection.
// Local spaces ("" Node) read as "local" rather than a blank heading. A
// single-group result (the common, local-only cockpit) tells renderSpaceList
// to skip the heading and render exactly as a flat table would.
func groupByNode(spaces []Space) []nodeGroup {
	var groups []nodeGroup
	index := map[string]int{}
	for i, sp := range spaces {
		name := sp.Node
		if name == "" {
			name = "local"
		}
		gi, ok := index[name]
		if !ok {
			gi = len(groups)
			index[name] = gi
			groups = append(groups, nodeGroup{node: name})
		}
		groups[gi].rows = append(groups[gi].rows, nodeRow{space: sp, index: i})
	}
	return groups
}

// spaceVolumes is the VOL cell, blanked for an unread row for the same reason
// spaceCount is: an unknown count must not render as a known zero.
func spaceVolumes(sp Space) string {
	if sp.Err != "" {
		return "—"
	}
	return itoa(len(sp.Volumes))
}

// repoColMinWidth is the terminal width from which the spaces table can afford
// a repo-path column without starving the name columns.
const repoColMinWidth = 120

// distribute sizes the flex columns. Each one asks for what its own content
// needs — stretching a 20-character name across 40 columns scatters the row —
// and the surplus goes to the LAST flex column so the table still spans the
// width instead of huddling on the left. When content asks for more than there
// is, every flex column shrinks to an equal share, floored at flexMinWidth, so
// a narrow terminal degrades legibly rather than collapsing.
func distribute(cols []Column, width int, rows []Row) []Column {
	fixed, flex, last := 0, 0, -1
	for i, c := range cols {
		if c.Flex {
			flex++
			last = i
			continue
		}
		fixed += c.Width
	}
	if flex == 0 {
		return cols
	}
	out := make([]Column, len(cols))
	copy(out, cols)

	avail := width - fixed - len(cols)*cellGap - tableIndent
	want := 0
	for i := range out {
		if out[i].Flex {
			out[i].Width = contentWidth(rows, i, out[i].Header)
			want += out[i].Width
		}
	}
	if want > avail {
		share := avail / flex
		if share < flexMinWidth {
			share = flexMinWidth
		}
		for i := range out {
			if out[i].Flex {
				out[i].Width = share
			}
		}
		return out
	}
	out[last].Width += avail - want
	return out
}

// contentWidth is the widest cell in column i, header included.
func contentWidth(rows []Row, i int, header string) int {
	w := len([]rune(header))
	for _, r := range rows {
		if i < len(r.Cells) {
			if n := len([]rune(r.Cells[i])); n > w {
				w = n
			}
		}
	}
	return w
}

const (
	flexMinWidth = 8
	tableIndent  = 2
)

// limitedInfoDetail states what a non-supervised space's rows do and do not
// mean: they are container state read from runc, so a status is a process fact,
// not a readiness check, and there are no captured logs. Counts and paths live in
// the table columns; keybindings live in the footer.
func limitedInfoDetail(sp *Space) string {
	detail := "no control API — container state only"
	if len(sp.Volumes) > 0 {
		detail += " — volumes: " + joinNames(sp.Volumes)
	}
	return detail
}

// spaceApp is the APP cell: which application a space belongs to. The space
// names are content hashes (sklp-dev-3aa69aa11788), so without this the list
// gives the operator no way to tell a shop space from a skalpai one.
//
// A worktree checkout lives at <app>-worktrees/<branch>, so the app name is one
// level up; the branch is appended when present, since several worktrees of the
// same app are otherwise indistinguishable.
func spaceApp(sp Space) string {
	app, worktree := splitRepoPath(sp.Repo)
	if app == "" {
		return "—"
	}
	if worktree == "" {
		return app
	}
	return app + " (" + worktree + ")"
}

// splitRepoPath maps a repo path to its app name and, for a worktree checkout,
// the branch dir. Returns ("", "") for an empty path.
func splitRepoPath(repo string) (app, worktree string) {
	repo = strings.TrimRight(repo, "/")
	if repo == "" {
		return "", ""
	}
	base := path.Base(repo)
	parent := path.Base(path.Dir(repo))
	if strings.HasSuffix(parent, worktreesDirSuffix) {
		return strings.TrimSuffix(parent, worktreesDirSuffix), base
	}
	return base, ""
}

// worktreesDirSuffix is the convention `sklp flow start` follows when it creates
// a feature worktree: <app>-worktrees/<branch>.
const worktreesDirSuffix = "-worktrees"

// spaceState picks the row glyph. Non-supervised rows reflect their runc status.
func spaceState(sp Space) State {
	switch {
	case sp.Err != "":
		return StateError
	case sp.LimitedInfo:
		return mapState(sp.Status)
	case !sp.AllReady:
		return StateBusy
	default:
		return StateReady
	}
}

// spaceCount is the SERVICES cell: service count when supervised, else the runc
// container count for a non-supervised space.
func spaceCount(sp Space) string {
	// A row that failed to read knows nothing about containers; "0 ctr" would
	// read as a machine running nothing, which is the opposite of unknown.
	if sp.Err != "" {
		return "—"
	}
	if sp.LimitedInfo {
		return itoa(sp.Containers) + " ctr"
	}
	return itoa(len(sp.Services))
}

// renderDetail draws the selected space's services — the master-detail right
// pane. v1 shows the flat services list (no dev/pipe/deploy split: the control
// state does not carry that classification).
// renderDetail draws the selected space's services. It returns "" when the
// spaces table already says everything there is to say, so an unsupervised
// selection does not push an empty frame onto the screen.
func renderDetail(m *Model, r Renderer, width int) string {
	sp := m.Selection()
	if sp == nil {
		return ""
	}
	if sp.Err != "" {
		return note(r, sp.Err, StyleErr, width)
	}
	if len(sp.Services) == 0 {
		if sp.LimitedInfo {
			return note(r, limitedInfoDetail(sp), StyleMuted, width)
		}
		return note(r, "space has no services", StyleMuted, width)
	}
	var b strings.Builder
	// The rows of a non-supervised space are container state, not application
	// readiness. Say so above the table: a ● here means the process runs, which is
	// a weaker claim than the ● of a supervised service that passed its check.
	if sp.LimitedInfo {
		b.WriteString(note(r, limitedInfoDetail(sp), StyleMuted, width))
		b.WriteString("\n")
	}
	rows := make([]Row, 0, len(sp.Services))
	for i, svc := range sp.Services {
		rows = append(rows, Row{
			Cells:    []string{r.StatusGlyph(svc.State), svc.Name, itoa(svc.Instances)},
			Selected: m.Level == LevelServices && i == m.SvcIndex,
			State:    svc.State,
		})
	}
	header := "SERVICE"
	if sp.LimitedInfo {
		header = "CONTAINER"
	}
	cols := []Column{
		{Header: "", Width: 1},
		{Header: header, Flex: true},
		{Header: "INST", Width: 4},
	}
	b.WriteString(r.Table(distribute(cols, width, rows), rows, width))
	return b.String()
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

func joinNames(names []string) string { return strings.Join(names, ", ") }

// killVolumeWarning spells out the data a teardown destroys with the space. The
// deletion is irreversible, so the prompt names the volumes rather than letting
// the operator discover them missing afterwards.
func killVolumeWarning(m *Model) string {
	sp := m.spaceByName(m.PendingKill)
	if sp == nil || len(sp.Volumes) == 0 {
		return ""
	}
	return " and its " + itoa(len(sp.Volumes)) + " volume(s) (" + joinNames(sp.Volumes) + ")"
}

// statusLabel humanises a runc-state status for the limited-info note.
func statusLabel(s string) string {
	if s == "" {
		return "status unknown"
	}
	return s
}
