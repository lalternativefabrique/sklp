package cockpit

import (
	"fmt"
	"strings"
	"testing"
)

// fakeRenderer records every port call and emits tagged text. It proves view.go
// touches ONLY the Renderer port (a compile-time + behavioral guarantee) and
// lets tests assert which components were composed.
type fakeRenderer struct {
	boxes  []string // box titles
	tables []tableCall
	glyphs []State
	// sbs records each SideBySide call as {left, right, leftWidth}.
	sbs []sbsCall
	// header/crumb record the last context block drawn.
	headerFields [][2]string
	headerKeys   [][2]string
	crumb        string
	// widths records the width every framed block was rendered at, so tests can
	// prove no block is drawn wider than the terminal.
	widths []int
}

// sbsCall records one SideBySide invocation for assertions.
type sbsCall struct {
	left      string
	right     string
	leftWidth int
}

// tableCall records one Table invocation so tests can assert on the columns and
// on which row carried the selection.
type tableCall struct {
	cols  []Column
	rows  []Row
	width int
}

// span is the width the columns actually cover, gaps and indent included.
func (t tableCall) span() int {
	n := tableIndent
	for _, c := range t.cols {
		n += c.Width + cellGap
	}
	return n
}

// selected returns the index of the selected row, or -1.
func (t tableCall) selected() int {
	for i, r := range t.rows {
		if r.Selected {
			return i
		}
	}
	return -1
}

func (t tableCall) headers() []string {
	out := make([]string, len(t.cols))
	for i, c := range t.cols {
		out[i] = c.Header
	}
	return out
}

func (f *fakeRenderer) Box(title, body string, width int) string {
	f.boxes = append(f.boxes, title)
	f.widths = append(f.widths, width)
	return "BOX<" + title + ">{" + body + "}"
}

func (f *fakeRenderer) Table(cols []Column, rows []Row, width int) string {
	f.tables = append(f.tables, tableCall{cols: cols, rows: rows, width: width})
	f.widths = append(f.widths, width)
	hdr := make([]string, len(cols))
	for i, c := range cols {
		hdr[i] = c.Header
	}
	return fmt.Sprintf("TABLE<%s|%drows>", strings.Join(hdr, ","), len(rows))
}

func (f *fakeRenderer) StatusGlyph(s State) string {
	f.glyphs = append(f.glyphs, s)
	return fmt.Sprintf("GLYPH<%d>", s)
}

func (f *fakeRenderer) Text(s string, _ Style) string { return s }
func (f *fakeRenderer) HRule(w int) string            { return "HR" }

func (f *fakeRenderer) Header(fields [][2]string, keys [][2]string, width int) string {
	f.headerFields, f.headerKeys = fields, keys
	f.widths = append(f.widths, width)
	var b strings.Builder
	b.WriteString("HEADER<")
	for _, kv := range fields {
		b.WriteString(kv[0] + " " + kv[1] + ";")
	}
	for _, kv := range keys {
		b.WriteString(kv[0] + " " + kv[1] + ";")
	}
	b.WriteString(">")
	return b.String()
}

func (f *fakeRenderer) Crumb(path, right string, width int) string {
	f.crumb = path
	f.widths = append(f.widths, width)
	return "CRUMB<" + path + "|" + right + ">"
}

func (f *fakeRenderer) SideBySide(left, right string, leftWidth int) string {
	f.sbs = append(f.sbs, sbsCall{left: left, right: right, leftWidth: leftWidth})
	return "SBS<" + left + "|" + right + ">"
}

// hasBox matches a box by title PREFIX: the logs panel appends its scroll
// position to the title, and callers assert on which box was drawn.
func (f *fakeRenderer) hasBox(title string) bool {
	for _, b := range f.boxes {
		if strings.HasPrefix(b, title) {
			return true
		}
	}
	return false
}

// lastTable returns the most recently drawn table.
func (f *fakeRenderer) lastTable() tableCall {
	if len(f.tables) == 0 {
		return tableCall{}
	}
	return f.tables[len(f.tables)-1]
}

func TestRenderEmptyStateShowsHint(t *testing.T) {
	f := &fakeRenderer{}
	out := Render(&Model{}, f)
	if !strings.Contains(out, "no spaces") {
		t.Fatalf("empty state missing hint:\n%s", out)
	}
}

func TestRenderNoFlowCollapsedBanner(t *testing.T) {
	f := &fakeRenderer{}
	out := Render(&Model{}, f)
	if !strings.Contains(out, "no active feature flow") {
		t.Fatalf("no-flow banner missing collapsed hint:\n%s", out)
	}
}

func TestRenderActiveFlow(t *testing.T) {
	f := &fakeRenderer{}
	m := &Model{Flow: FlowState{Branch: "feat/login", Onto: "main", Changed: 3, Start: MilestoneDone, Coding: MilestoneActive}}
	out := Render(m, f)
	if !strings.Contains(out, "feat/login") || !strings.Contains(out, "3 file(s)") {
		t.Fatalf("active flow banner missing branch/diff:\n%s", out)
	}
}

func TestRenderMasterDetail(t *testing.T) {
	f := &fakeRenderer{}
	m := &Model{
		Spaces: []Space{
			{Name: "alpha", AllReady: true, Services: []Service{{Name: "api", State: StateReady, Instances: 2}}},
			{Name: "bravo", AllReady: false},
		},
		Host: HostRollup{Spaces: 2, Services: 1, Ready: 1},
	}
	out := Render(m, f)

	if len(f.tables) < 2 {
		t.Fatalf("expected list + detail tables, got %d", len(f.tables))
	}
	if !strings.Contains(out, "2 · 0 supervised") {
		t.Fatalf("missing host roll-up:\n%s", out)
	}
	if got := f.tables[0].selected(); got != 0 {
		t.Fatalf("first space must be selected, got row %d", got)
	}
}

func TestRenderShowsSupervisedAndNonSupervised(t *testing.T) {
	f := &fakeRenderer{}
	m := &Model{
		Spaces: []Space{
			{Name: "alpha", Supervised: true, AllReady: true, Services: []Service{{Name: "api", State: StateReady, Instances: 1}}},
			{Name: "dd", LimitedInfo: true, Status: "running", Containers: 2},
		},
		Host: rollup([]Space{
			{Name: "alpha", Supervised: true, AllReady: true, Services: []Service{{Name: "api", State: StateReady}}},
			{Name: "dd", LimitedInfo: true, Status: "running", Containers: 2},
		}),
	}
	out := Render(m, f)

	// Both spaces must appear in the list (2 rows).
	if !strings.Contains(out, "2rows") {
		t.Fatalf("space list should hold both spaces:\n%s", out)
	}
	// Host roll-up honestly counts total vs supervised.
	if !strings.Contains(out, "2 · 1 supervised") {
		t.Fatalf("roll-up must show 1 of 2 supervised:\n%s", out)
	}

	// Selecting the non-supervised space shows the limited-info note, no services.
	m.Selected = 1
	f2 := &fakeRenderer{}
	out = Render(m, f2)
	if !strings.Contains(out, "no control API") {
		t.Fatalf("non-supervised detail must state why the service list is absent:\n%s", out)
	}
	for _, tbl := range f2.tables {
		if strings.Join(tbl.headers(), ",") == ",SERVICE,INST" {
			t.Fatalf("non-supervised space must not render a services table:\n%s", out)
		}
	}
}

func TestRenderErrorBanner(t *testing.T) {
	f := &fakeRenderer{}
	out := Render(&Model{Err: "list failed"}, f)
	if !f.hasBox("Error") || !strings.Contains(out, "list failed") {
		t.Fatalf("error state not rendered:\n%s", out)
	}
}

func TestRenderFilterHintShown(t *testing.T) {
	f := &fakeRenderer{}
	m := &Model{
		Spaces: []Space{{Name: "alpha", AllReady: true}},
		Filter: FilterDev,
	}
	out := Render(m, f)
	if !strings.Contains(out, "no-op") {
		t.Fatalf("active filter should surface the honest no-op hint:\n%s", out)
	}
}

func TestRenderLogsPanelOpen(t *testing.T) {
	f := &fakeRenderer{}
	m := &Model{
		Spaces:      []Space{{Name: "alpha", Supervised: true, Services: []Service{{Name: "api", State: StateReady}}}},
		LogsOpen:    true,
		LogsService: "api",
		LogsSpace:   "alpha",
		LogsScroll:  -1,
		LogsLines:   []string{"hello from api"},
	}
	out := Render(m, f)
	if !f.hasBox("logs: api") {
		t.Fatalf("open panel should draw a 'logs: api' box; boxes=%v", f.boxes)
	}
	if !strings.Contains(out, "hello from api") {
		t.Fatalf("panel should render the streamed line:\n%s", out)
	}
	if !strings.Contains(out, "[l/esc] close") {
		t.Fatalf("panel should render the close hint:\n%s", out)
	}
}

func TestRenderLogsPanelClosed(t *testing.T) {
	f := &fakeRenderer{}
	m := &Model{Spaces: []Space{{Name: "alpha", Supervised: true, Services: []Service{{Name: "api"}}}}}
	out := Render(m, f)
	if f.hasBox("logs: api") {
		t.Fatal("closed panel should not draw a logs box")
	}
	if strings.Contains(out, "[l/esc] close") {
		t.Fatalf("closed view should not show the panel hint:\n%s", out)
	}
}

func TestRenderLogsSplitSideBySide(t *testing.T) {
	f := &fakeRenderer{}
	m := &Model{
		Spaces:      []Space{{Name: "alpha", Supervised: true, Services: []Service{{Name: "api", State: StateReady}}}},
		Width:       120,
		Height:      40,
		LogsOpen:    true,
		LogsService: "api",
		LogsSpace:   "alpha",
		LogsScroll:  -1,
		LogsLines:   []string{"hello from api"},
	}
	Render(m, f)
	if len(f.sbs) != 1 {
		t.Fatalf("open panel with known width should call SideBySide once, got %d", len(f.sbs))
	}
	call := f.sbs[0]
	if !strings.Contains(call.left, "CRUMB<spaces(1)") {
		t.Fatalf("left arg should hold the cockpit body:\n%s", call.left)
	}
	// Left column carries the cockpit's own footer, never the scroll/close one.
	for _, tok := range []string{"filter", "sort", "quit"} {
		if !strings.Contains(call.left, tok) {
			t.Fatalf("left footer should be the cockpit keybindings (missing %q):\n%s", tok, call.left)
		}
	}
	if strings.Contains(call.left, "[l/esc] close") {
		t.Fatalf("scroll/close footer must not leak into the left column:\n%s", call.left)
	}
	// Right column carries the logs panel and only the scroll/close footer.
	if !strings.Contains(call.right, "logs: api") {
		t.Fatalf("right arg should hold the logs panel:\n%s", call.right)
	}
	if !strings.Contains(call.right, "[l/esc] close") {
		t.Fatalf("right footer should be the scroll/close hint:\n%s", call.right)
	}
	if call.leftWidth != cockpitWidth(120) {
		t.Fatalf("leftWidth should be the computed cockpit column, got %d", call.leftWidth)
	}
}

func TestRenderClosedDoesNotSplit(t *testing.T) {
	f := &fakeRenderer{}
	m := &Model{
		Spaces: []Space{{Name: "alpha", Supervised: true, Services: []Service{{Name: "api"}}}},
		Width:  120,
	}
	Render(m, f)
	if len(f.sbs) != 0 {
		t.Fatalf("closed panel must not call SideBySide, got %d", len(f.sbs))
	}
}

func TestPlainSideBySide(t *testing.T) {
	r := NewPlainRenderer()
	got := r.SideBySide("aa\nb", "1\n2\n3", 4)
	want := "aa   │ 1\nb    │ 2\n     │ 3"
	if got != want {
		t.Fatalf("plain SideBySide mismatch:\ngot=%q\nwant=%q", got, want)
	}
}

func TestRenderServicesLevelCursorAndFooter(t *testing.T) {
	m := &Model{
		Spaces: []Space{{
			Name: "demo", Supervised: true, AllReady: true,
			Services: []Service{
				{Name: "caddy", State: StateReady},
				{Name: "mysql", State: StateReady},
				{Name: "nginx", State: StateReady},
			},
		}},
		Level:    LevelServices,
		SvcIndex: 1,
	}
	f := &fakeRenderer{}
	out := Render(m, f)
	// The selection must mark the highlighted (mysql, index 1) row, not caddy.
	if got := f.lastTable().selected(); got != 1 {
		t.Fatalf("services level should select the highlighted service, got row %d", got)
	}
	if !strings.Contains(out, "pick svc") || !strings.Contains(out, "[esc] back") {
		t.Fatalf("services-level footer missing:\n%s", out)
	}
	if strings.Contains(out, "[f] filter") {
		t.Fatalf("services level should not show the spaces footer:\n%s", out)
	}
}

func TestRenderSpacesLevelSelectsNoService(t *testing.T) {
	m := &Model{
		Spaces: []Space{{
			Name: "demo", Supervised: true, AllReady: true,
			Services: []Service{{Name: "caddy", State: StateReady}},
		}},
		Level: LevelSpaces,
	}
	f := &fakeRenderer{}
	out := Render(m, f)
	if got := f.lastTable().selected(); got != -1 {
		t.Fatalf("spaces level must not highlight a service row, got %d", got)
	}
	if !strings.Contains(out, "[f] filter") || !strings.Contains(out, "[↵] enter") {
		t.Fatalf("spaces-level footer missing:\n%s", out)
	}
}

func TestRenderSelectedSpaceErrorDetail(t *testing.T) {
	f := &fakeRenderer{}
	m := &Model{Spaces: []Space{{Name: "alpha", Err: "socket died"}}}
	out := Render(m, f)
	if !strings.Contains(out, "socket died") {
		t.Fatalf("per-space error not shown in detail:\n%s", out)
	}
}

func TestRenderPendingKillPrompt(t *testing.T) {
	m := &Model{
		Spaces:      []Space{{Name: "bare", LimitedInfo: true, Status: "running"}},
		PendingKill: "bare",
	}
	out := Render(m, NewPlainRenderer())
	if !strings.Contains(out, "delete space bare? [↵/y] confirm  [n] cancel") {
		t.Fatalf("confirm prompt missing:\n%s", out)
	}
}

func TestLogsHeightFor(t *testing.T) {
	cases := map[int]int{
		0:  logsPanelHeight, // unknown -> fallback
		40: 40 - logsChrome, // fills the screen minus the pane's own chrome
		6:  5,               // floored at minimum
		3:  5,               // tiny terminal still shows the floor
	}
	for term, want := range cases {
		if got := logsHeightFor(term); got != want {
			t.Errorf("logsHeightFor(%d) = %d, want %d", term, got, want)
		}
	}
}

func TestCockpitWidth_LogsPaneGetsTheLargerShare(t *testing.T) {
	const term = 200
	left := cockpitWidth(term)
	right := term - left - splitGutter
	if left >= right {
		t.Errorf("logs pane must be the wider column: left=%d right=%d", left, right)
	}
	if left+right+splitGutter != term {
		t.Errorf("split must consume the terminal exactly: %d+%d+%d != %d", left, right, splitGutter, term)
	}
}

// The cockpit used to render at a fixed 80 columns whatever the terminal was,
// so a wide screen sat two-thirds empty.
func TestRenderUsesTheFullTerminalWidth(t *testing.T) {
	f := &fakeRenderer{}
	m := &Model{Spaces: []Space{{Name: "alpha", AllReady: true}}, Width: 200}
	Render(m, f)
	for _, w := range f.widths {
		if w != 200 {
			t.Fatalf("every block must be drawn at the terminal width, got %d (all: %v)", w, f.widths)
		}
	}
}

// Framed blocks used to be clipped AFTER rendering, which shears a border
// mid-line. Every block must instead be told its width up front, and the two
// split columns must fit the terminal exactly.
func TestRenderSplitNeverExceedsTerminalWidth(t *testing.T) {
	f := &fakeRenderer{}
	m := &Model{
		Spaces:    []Space{{Name: "alpha", AllReady: true}},
		Width:     160,
		LogsOpen:  true,
		LogsSpace: "alpha",
		LogsLines: []string{"line"},
	}
	Render(m, f)
	if len(f.sbs) != 1 {
		t.Fatalf("expected one side-by-side join, got %d", len(f.sbs))
	}
	left := f.sbs[0].leftWidth
	for _, w := range f.widths {
		if w != left && w != 160-left-splitGutter {
			t.Fatalf("block width %d matches neither column (left=%d), so it would be clipped", w, left)
		}
	}
}

// The table used to be sized to its content only, so on a wide terminal it
// huddled on the left and the selection background stopped mid-screen, reading
// as a stray block rather than as the current row.
func TestSpacesTableSpansTheWidth(t *testing.T) {
	f := &fakeRenderer{}
	m := &Model{
		Spaces: []Space{
			{Name: "ci-acme-fb9ebc9c", Repo: "/home/u/code/projects/acme", LimitedInfo: true, Containers: 3},
			{Name: "sklp-dev-f09cf69923a2", Repo: "/home/u/code/projects/waves", LimitedInfo: true, Containers: 1},
		},
		Width: 200,
	}
	Render(m, f)
	spaces := f.tables[0]
	if spaces.width != 200 {
		t.Fatalf("table must be painted across the terminal, got %d", spaces.width)
	}
	if got := spaces.span(); got > 200 || got < 200-cellGap*len(spaces.cols) {
		t.Fatalf("columns should consume the width, span=%d of 200", got)
	}
}

// A log line longer than the panel wraps inside the box, so windowing on raw
// buffer lines overflows the terminal and the panel scrolls past its own end.
func TestLogsWindowCountsWrappedRows(t *testing.T) {
	long := strings.Repeat("x", 250)
	rows := wrapLines([]string{long, "short"}, 100)
	if len(rows) != 4 {
		t.Fatalf("250 chars at width 100 = 3 rows, plus 1 short = 4, got %d", len(rows))
	}
	for i, r := range rows {
		if len([]rune(r)) > 100 {
			t.Fatalf("row %d is %d wide, must not exceed the panel", i, len([]rune(r)))
		}
	}
}

// Below the split threshold two ~40-column panes serve nobody, so the logs take
// over the screen instead.
func TestRenderNarrowTerminalGivesLogsTheWholeScreen(t *testing.T) {
	f := &fakeRenderer{}
	m := &Model{
		Spaces:    []Space{{Name: "alpha", AllReady: true}},
		Width:     splitMinWidth - 1,
		LogsOpen:  true,
		LogsSpace: "alpha",
		LogsLines: []string{"line"},
	}
	Render(m, f)
	if len(f.sbs) != 0 {
		t.Fatalf("narrow terminal must not split, got %d joins", len(f.sbs))
	}
}

// Space names are content hashes, so the list must say which app a space serves
// or the operator cannot tell a shop space from a skalpai one.
func TestSpaceApp(t *testing.T) {
	cases := []struct {
		repo string
		want string
	}{
		{"/home/u/code/projects/shop", "shop"},
		{"/home/u/code/projects/shop/", "shop"},
		{"/home/u/code/projects/shop-worktrees/feat-webhooks-ui", "shop (feat-webhooks-ui)"},
		{"/home/u/code/projects/skalpai-worktrees/feat-clean", "skalpai (feat-clean)"},
		{"", "—"},
	}
	for _, tc := range cases {
		t.Run(tc.repo, func(t *testing.T) {
			if got := spaceApp(Space{Repo: tc.repo}); got != tc.want {
				t.Errorf("spaceApp(%q) = %q, want %q", tc.repo, got, tc.want)
			}
		})
	}
}

// A non-supervised space's facts belong in the table columns; the note carries
// only what the columns cannot say. The facts must still reach the screen.
func TestNonSupervisedSpaceFactsAreInTheTable(t *testing.T) {
	f := &fakeRenderer{}
	m := &Model{Spaces: []Space{{
		Name:        "dd",
		Repo:        "/home/u/code/projects/shop-worktrees/feat-webhooks-ui",
		LimitedInfo: true,
		Status:      "running",
		Containers:  4,
		Volumes:     []string{"pgdata"},
	}}}
	out := Render(m, f)

	row := strings.Join(f.tables[0].rows[0].Cells, "|")
	for _, want := range []string{"dd", "shop (feat-webhooks-ui)", "4 ctr", "1"} {
		if !strings.Contains(row, want) {
			t.Errorf("spaces row missing %q: %s", want, row)
		}
	}
	if !strings.Contains(out, "pgdata") {
		t.Errorf("volumes at stake on a delete must be named:\n%s", out)
	}
}

// The note states the ONE thing the columns cannot: the service list is
// unavailable rather than empty.
func TestLimitedInfoDetailStatesTheMissingAPI(t *testing.T) {
	got := limitedInfoDetail(&Space{Status: "running", Containers: 4, Volumes: []string{"pgdata"}})
	if !strings.Contains(got, "no control API") {
		t.Errorf("the missing-API caveat must survive: %s", got)
	}
	if !strings.Contains(got, "pgdata") {
		t.Errorf("volumes at stake must be named: %s", got)
	}
}

func TestLimitedInfoDetailWithoutRepo(t *testing.T) {
	got := limitedInfoDetail(&Space{Status: "", Containers: 0})
	if strings.Contains(got, "app:") {
		t.Errorf("no repo must not fabricate an app line: %s", got)
	}
	if !strings.Contains(got, "no control API") {
		t.Errorf("the missing-API caveat must survive: %s", got)
	}
}

// longestFreeLine is the widest line of a rendered block, ignoring the lines the
// fake renderer produces for width-aware blocks (header, crumb, table, box):
// those receive an explicit width by contract, and the fake ignores it. What is
// measured here is the free-standing text view.go emits itself.
func longestFreeLine(s string) int {
	w := 0
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, "HEADER<") || strings.HasPrefix(l, "CRUMB<") ||
			strings.HasPrefix(l, "TABLE<") || strings.HasPrefix(l, "BOX<") {
			continue
		}
		if n := len([]rune(l)); n > w {
			w = n
		}
	}
	return w
}

// A free-standing cockpit line (the non-supervised volume list) used to be
// rendered without a width, so it escaped its column. SideBySide aligns the right
// pane on the WIDEST left line, so that one line squeezed the logs pane down to a
// handful of columns and every log line wrapped into fragments.
func TestLogsPaneKeepsItsWidthAgainstALongCockpitLine(t *testing.T) {
	f := &fakeRenderer{}
	m := &Model{
		Spaces: []Space{{
			Name:        "sklp-dev-71ed3254bb55",
			Repo:        "/home/u/code/projects/shop",
			LimitedInfo: true,
			Containers:  5,
			Volumes:     []string{"_anon-POSTGRES-VAR_LIB_POSTGRESQL_DATA"},
		}},
		Width:     200,
		Height:    40,
		LogsOpen:  true,
		LogsSpace: "sklp-dev-71ed3254bb55",
		LogsLines: []string{"line"},
	}
	Render(m, f)

	if len(f.sbs) != 1 {
		t.Fatalf("expected one side-by-side join, got %d", len(f.sbs))
	}
	left := f.sbs[0].leftWidth
	if got := longestFreeLine(f.sbs[0].left); got > left {
		t.Errorf("cockpit column overflows its %d columns (widest line %d), which collapses the logs pane", left, got)
	}
	if right := 200 - left - splitGutter; right < logsMinWidth {
		t.Errorf("logs pane got %d columns, below the %d floor", right, logsMinWidth)
	}
}

// The logs pane is the column that must not be starved: log lines carry long
// identifiers, while the spaces table degrades by dropping columns.
func TestCockpitWidthFloorsTheLogsPane(t *testing.T) {
	for _, term := range []int{splitMinWidth, 120, 160, 200, 400} {
		left := cockpitWidth(term)
		right := term - left - splitGutter
		if right < logsMinWidth {
			t.Errorf("width %d: logs pane %d below the %d floor", term, right, logsMinWidth)
		}
		if left < cockpitMinWidth {
			t.Errorf("width %d: cockpit column %d below the %d floor", term, left, cockpitMinWidth)
		}
		if left+right+splitGutter != term {
			t.Errorf("width %d: split must consume the terminal exactly, got %d+%d+%d", term, left, right, splitGutter)
		}
	}
}

// The status line carries the delete confirm, which names every volume it is
// about to destroy — easily wider than the cockpit column.
func TestPendingKillPromptStaysInsideItsColumn(t *testing.T) {
	f := &fakeRenderer{}
	m := &Model{
		Spaces: []Space{{
			Name:        "sklp-dev-71ed3254bb55",
			LimitedInfo: true,
			Volumes:     []string{"_anon-POSTGRES-VAR_LIB_POSTGRESQL_DATA", "_anon-NATS-VAR_LIB_NATS_STORE"},
		}},
		Width:       200,
		Height:      40,
		PendingKill: "sklp-dev-71ed3254bb55",
		LogsOpen:    true,
		LogsSpace:   "sklp-dev-71ed3254bb55",
		LogsLines:   []string{"line"},
	}
	Render(m, f)
	if len(f.sbs) != 1 {
		t.Fatalf("expected one side-by-side join, got %d", len(f.sbs))
	}
	if got, left := longestFreeLine(f.sbs[0].left), f.sbs[0].leftWidth; got > left {
		t.Errorf("confirm prompt overflows its %d columns (widest line %d)", left, got)
	}
}

// The panel footer advertises copy, since a mouse selection in the alt-screen
// only ever grabs the wrapped fragments.
func TestLogsFooterAdvertisesCopy(t *testing.T) {
	f := &fakeRenderer{}
	m := &Model{
		Spaces:    []Space{{Name: "alpha", AllReady: true}},
		Width:     160,
		Height:    40,
		LogsOpen:  true,
		LogsSpace: "alpha",
		LogsLines: []string{"line"},
	}
	out := Render(m, f)
	if !strings.Contains(out, "[c] copy") {
		t.Errorf("logs footer must advertise the copy action: %s", out)
	}
}

// The copy report costs a screen row, so the log window must give one back or the
// panel runs past the bottom of the terminal.
func TestCopyNoteShrinksTheLogWindow(t *testing.T) {
	plain := logsHeight(40, 0)
	withNote := logsHeight(40, 1)
	if withNote != plain-1 {
		t.Errorf("the note row must come out of the log window: %d vs %d", withNote, plain)
	}
}

// Left to the wrapper, the footer broke mid-word ("[d] de / lete"), orphaning a
// key from the verb that explains it.
func TestNarrowFooterBreaksBetweenBindings(t *testing.T) {
	m := &Model{}
	for _, line := range strings.Split(cockpitFooter(m, footerAbbrevWidth-1), "\n") {
		if len([]rune(line)) > footerAbbrevWidth-1-noteIndent {
			t.Errorf("footer line still needs wrapping: %q", line)
		}
		if strings.HasSuffix(line, "[") || strings.HasPrefix(line, "]") {
			t.Errorf("footer line splits a binding: %q", line)
		}
	}
	if got := cockpitFooter(m, 200); strings.Contains(got, "\n") {
		t.Errorf("a wide column keeps the footer on one line: %q", got)
	}
}

// The search prompt is a row below the log box, so the log window must shrink by
// it — otherwise the pane grows past the bottom of the screen the moment a
// search opens.
func TestSearchPromptKeepsThePaneInsideTheScreen(t *testing.T) {
	const height = 24
	m := &Model{Width: 100, Height: height, LogsOpen: true, LogsScroll: -1, SearchMatch: -1}
	for i := 0; i < 200; i++ {
		m.AppendLogLine("line")
	}
	before := strings.Count(renderLogsPane(m, NewPlainRenderer(), logsPaneWidth(m)), "\n")
	m.StartSearch()
	m.SearchInput('l')
	after := strings.Count(renderLogsPane(m, NewPlainRenderer(), logsPaneWidth(m)), "\n")
	if after != before {
		t.Fatalf("pane grew from %d to %d rows when the search opened", before, after)
	}
	if after >= height {
		t.Fatalf("pane is %d rows for a %d-row terminal", after, height)
	}
}

func TestSearchPromptShowsTheQuery(t *testing.T) {
	m := &Model{Width: 100, Height: 24, LogsOpen: true, LogsScroll: -1, SearchMatch: -1}
	m.AppendLogLine("boom")
	m.StartSearch()
	m.SearchInput('b')
	if got := searchPrompt(m); !strings.Contains(got, "/b") {
		t.Fatalf("searchPrompt = %q, want it to show the live query", got)
	}
	m.CommitSearch()
	if got := searchPrompt(m); !strings.Contains(got, "[n/N]") {
		t.Fatalf("searchPrompt = %q, want the committed-query hints", got)
	}
	m.ClearSearch()
	if got := searchPrompt(m); got != "" {
		t.Fatalf("searchPrompt = %q, want empty with no search", got)
	}
}

// A log line carries its own ANSI colors. Wrapping a style around them died at
// their first reset, so only the text past the LAST reset — the continuation row
// of a wrapped line — came out marked, and the match itself looked unhighlighted.
func TestFocusedMatchIsMarkedOnEveryRowOfAWrappedColouredLine(t *testing.T) {
	m := &Model{Width: 70, Height: 20, LogsOpen: true, LogsScroll: -1, SearchMatch: -1}
	coloured := "\x1b[36m[core]\x1b[0m INFO Starting consumer: linker-request " +
		"\x1b[32m[integration-events]\x1b[0m and more text to force a wrap here"
	m.AppendLogLine("plain line before")
	m.AppendLogLine(coloured)
	m.AppendLogLine("plain line after")

	m.StartSearch()
	for _, r := range "linker" {
		m.SearchInput(r)
	}
	m.CommitSearch()
	if m.FocusedLine() != 1 {
		t.Fatalf("FocusedLine = %d, want 1", m.FocusedLine())
	}

	rows := m.logRows()
	if got := rowsFor(coloured, logsTextWidth(m)); got < 2 {
		t.Fatalf("the fixture must wrap: %d row(s)", got)
	}
	height := logsHeight(m.Height, logsPaneExtraRows(m))
	top := visibleTop(len(rows), m.LogsScroll, height)
	marked := markMatch(m, NewPlainRenderer(), visibleLogLines(rows, m.LogsScroll, height), top)

	start := m.bufferLineToRow(1)
	end := start + rowsFor(coloured, logsTextWidth(m))
	for i, l := range marked {
		row := top + i
		want := row >= start && row < end
		if got := strings.HasPrefix(l, matchGutterMark); got != want {
			t.Errorf("row %d: marked=%v, want %v (%q)", row, got, want, l)
		}
		if strings.Contains(l, "\x1b[") && want {
			t.Errorf("row %d still carries the log's own escapes: %q", row, l)
		}
	}
}

// The gutter column is reserved in logsTextWidth at all times, so it must be
// painted with no search too — otherwise every line shifts sideways the moment
// a search starts.
func TestGutterIsPaintedWithoutASearch(t *testing.T) {
	m := &Model{Width: 90, Height: 20, LogsOpen: true, LogsScroll: -1, SearchMatch: -1}
	m.AppendLogLine("a line")
	rows := m.logRows()
	height := logsHeight(m.Height, logsPaneExtraRows(m))
	out := markMatch(m, NewPlainRenderer(), visibleLogLines(rows, m.LogsScroll, height), 0)
	for i, l := range out {
		if !strings.HasPrefix(l, matchGutterBlank) {
			t.Errorf("row %d lacks the gutter column: %q", i, l)
		}
	}
}

func TestStripANSI(t *testing.T) {
	for in, want := range map[string]string{
		"\x1b[36m[core]\x1b[0m ready": "[core] ready",
		"plain":                       "plain",
		"\x1b[1;32mbold green\x1b[0m": "bold green",
	} {
		if got := stripANSI(in); got != want {
			t.Errorf("stripANSI(%q) = %q, want %q", in, got, want)
		}
	}
}

// A single-node cockpit renders one flat table with no node heading and no
// NODE column: on a workstation with nothing configured remotely, either would
// be redundant chrome around an all-local list.
func TestRenderSingleNodeHasNoHeadingOrColumn(t *testing.T) {
	local := []Space{{Name: "alpha", LimitedInfo: true, Status: "running", Containers: 1}}

	f := &fakeRenderer{}
	out := Render(&Model{Spaces: local, Host: rollup(local)}, f)
	if len(f.tables) < 1 || strings.Join(f.tables[0].headers(), ",") != ",SPACE,APP,SVC,VOL" {
		t.Fatalf("local-only cockpit must render one plain spaces table:\n%s", out)
	}
	for _, tbl := range f.tables {
		for _, h := range tbl.headers() {
			if h == "NODE" {
				t.Fatal("a local-only cockpit must not render a NODE column")
			}
		}
	}
	if strings.Contains(out, "NODE ·") {
		t.Fatalf("a local-only cockpit must not render a node heading:\n%s", out)
	}
}

// Spaces on different nodes render as SEPARATE tables under a "NODE · <name>"
// heading — a node is the machine a group of rows runs on, not a per-row
// attribute, and two nodes can list a same-named space. Grouping removes that
// ambiguity structurally instead of asking the reader to compare a text cell.
func TestRenderGroupsSpacesByNode(t *testing.T) {
	spaces := []Space{
		{Name: "alpha", LimitedInfo: true, Status: "running", Containers: 1},
		{Name: "production", Node: "acme-vm", LimitedInfo: true, Status: "running", Containers: 2},
	}
	f := &fakeRenderer{}
	out := Render(&Model{Spaces: spaces, Host: rollup(spaces)}, f)

	if !strings.Contains(out, "NODE · local") || !strings.Contains(out, "NODE · acme-vm") {
		t.Fatalf("both node headings must appear:\n%s", out)
	}
	if len(f.tables) < 2 {
		t.Fatalf("each node must draw its own table, got %d tables:\n%s", len(f.tables), out)
	}
	for _, tbl := range f.tables[:2] {
		for _, h := range tbl.headers() {
			if h == "NODE" {
				t.Fatal("grouping replaces the NODE column, it must not also add one")
			}
		}
	}
	if f.tables[0].rows[0].Cells[1] != "alpha" || f.tables[1].rows[0].Cells[1] != "production" {
		t.Fatalf("each group must list only its own node's space:\n%s", out)
	}
}

// Two same-named spaces on different nodes must not collide: grouping puts
// them in separate tables, and the selection (a flat index into m.Spaces)
// must still mark the right row in the right group.
func TestRenderGroupsSelectionSurvivesInterleavedNodes(t *testing.T) {
	spaces := []Space{
		{Name: "production", Node: "acme-vm", LimitedInfo: true, Status: "running"},
		{Name: "production", LimitedInfo: true, Status: "running"}, // local, sorts after by name-tie insertion order
	}
	m := &Model{Spaces: spaces, Host: rollup(spaces), Selected: 1}
	f := &fakeRenderer{}
	out := Render(m, f)

	marked := 0
	var selectedName string
	for _, tbl := range f.tables {
		if i := tbl.selected(); i >= 0 {
			marked++
			selectedName = tbl.rows[i].Cells[1]
		}
	}
	if marked != 1 {
		t.Fatalf("exactly one group's table should carry the selection, got %d:\n%s", marked, out)
	}
	if selectedName != "production" {
		t.Fatalf("selected row should be the local 'production' space (index 1), got %q:\n%s", selectedName, out)
	}
}
