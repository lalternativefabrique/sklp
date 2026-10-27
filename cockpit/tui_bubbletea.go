package cockpit

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// THIS IS THE ONLY FILE THAT MAY IMPORT bubbletea/lipgloss. It implements the
// App (event loop) and Renderer (lipgloss draw) ports; all cockpit logic lives
// in model/view/state. Swapping to another TUI library = replacing only this.

// refreshInterval is how often the cockpit re-reads real state.
const refreshInterval = 2 * time.Second

// BubbleApp is the bubbletea-backed App.
type BubbleApp struct{}

// NewBubbleApp returns the default App implementation.
func NewBubbleApp() App { return BubbleApp{} }

// Run drives the bubbletea program until quit or ctx cancel.
func (BubbleApp) Run(ctx context.Context, m *Model, r Renderer) error {
	p := tea.NewProgram(&teaModel{m: m, r: r}, tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := p.Run()
	return err
}

// tickMsg drives the periodic state refresh.
type tickMsg struct{}

func tick() tea.Cmd {
	return tea.Tick(refreshInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

// snapshotMsg carries a state read that completed off the event loop.
type snapshotMsg struct{ snap Snapshot }

// readSnapshot runs the blocking source reads in bubbletea's own goroutine, so
// a slow or unreachable remote node delays only the next data update — not key
// handling and not log rendering.
func readSnapshot(node string) tea.Cmd {
	return func() tea.Msg { return snapshotMsg{snap: ReadSnapshot(context.Background(), node)} }
}

// teaModel adapts our Model+Renderer to the bubbletea Model interface. It holds
// no business logic beyond the async log-stream glue: keys map to our Key and
// delegate to Model.Update / Model.Refresh; rendering delegates to view.Render.
type teaModel struct {
	m *Model
	r Renderer
	// logCancel stops the active follow stream; nil when the panel is closed.
	logCancel context.CancelFunc
}

// logLineMsg carries streamed lines into the UI goroutine (no data race: the
// model is mutated only here, inside Update). Lines arrive in BATCHES: opening
// a panel replays the service's whole history, and delivering that one line per
// message meant one redraw per line — the panel scrolled through thousands of
// frames before it settled, which is what made opening a busy log look like an
// endless auto-scroll.
type logLineMsg struct{ lines []string }

// logBatchCap bounds one batch. Draining without a bound would let a fast
// producer starve key handling; a few hundred lines is one screen's worth many
// times over, so the cap costs nothing visually.
const logBatchCap = 512

// logErrMsg surfaces a stream failure into the panel.
type logErrMsg struct{ err string }

func (t *teaModel) Init() tea.Cmd { return tick() }

func (t *teaModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		// The jump palette swallows printable keys the same way a search prompt
		// does, and takes priority: Ctrl+K can open it from anywhere, including
		// while a search query is mid-type in the logs panel.
		if t.m.JumpOpen && t.typeIntoJump(msg) {
			return t, nil
		}
		// A search prompt swallows printable keys: a query containing g, c or q
		// must not fire those bindings mid-word.
		if t.m.SearchTyping && t.typeIntoSearch(msg) {
			return t, nil
		}
		k := mapKey(msg)
		// n/N step through search matches while the panel is open; elsewhere n
		// stays the cancel key for a pending confirm.
		if t.m.LogsOpen {
			switch msg.String() {
			case "n":
				k = KeyNext
			case "N":
				k = KeyPrev
			case "/":
				k = KeySearch
			}
		}
		// Esc (KeyBack) closes the panel when open, then steps back a level; it
		// never quits. q/Ctrl-C (KeyQuit) always quit.
		if k == KeyBack && t.m.LogsOpen {
			t.stopLogs()
			t.m.CloseLogs()
			return t, nil
		}
		if k == KeyQuit {
			t.stopLogs()
			return t, tea.Quit
		}
		wasOpen, prevSvc, wasCI, wasVM := t.m.LogsOpen, t.m.LogsService, t.m.LogsCI, t.m.LogsVM
		wasPicking := t.m.ContextsOpen
		t.m.Update(k)
		if wasPicking && !t.m.ContextsOpen && !t.m.JumpOpen {
			// A context was just confirmed: its inventory is empty until read,
			// so read now instead of leaving a blank list until the next tick.
			return t, readSnapshot(t.m.Context)
		}
		if req := t.m.TakePendingRestart(); req != nil {
			// Same hand-over as the shell: `sklp dev` is a foreground supervisor and
			// needs the terminal the cockpit is holding.
			t.stopLogs()
			t.m.CloseLogs()
			return t, execRestart(*req)
		}
		if req := t.m.TakePendingExec(); req != nil {
			// The shell needs the terminal the cockpit is holding: stop the stream
			// (its writes would land in the shell) and hand the screen to tea.Exec,
			// which restores the alt-screen when the shell exits.
			t.stopLogs()
			t.m.CloseLogs()
			return t, execShell(*req)
		}
		if cmd := t.syncLogStream(wasOpen, prevSvc, wasCI, wasVM); cmd != nil {
			return t, cmd
		}
		return t, nil
	case restartDoneMsg:
		if msg.err != "" {
			t.m.Note = "relaunch failed: " + msg.err
		} else {
			t.m.Note = "dev stack for " + msg.space + " exited"
		}
		t.m.Refresh(context.Background())
		return t, nil
	case execDoneMsg:
		if msg.err != "" {
			t.m.Note = "exec failed: " + msg.err
		} else {
			t.m.Note = "left " + msg.label
		}
		t.m.Refresh(context.Background())
		return t, nil
	case seqMsg:
		next := msg.next
		if m, cmd := t.Update(msg.msg); cmd != nil {
			_ = m
			return t, tea.Batch(cmd, next)
		}
		return t, next
	case logLineMsg:
		if t.m.LogsOpen {
			for _, l := range msg.lines {
				t.m.AppendLogLine(l)
			}
		}
		return t, nil
	case logErrMsg:
		if t.m.LogsOpen {
			t.m.LogsErr = msg.err
		}
		return t, nil
	case tea.WindowSizeMsg:
		t.m.Width = msg.Width
		t.m.Height = msg.Height
		return t, nil
	case tickMsg:
		// Only the read is scheduled here; the next tick is armed when the
		// snapshot lands, so a slow node cannot queue overlapping reads.
		if t.m.ContextsOpen {
			// No context chosen yet: reading would dial a node the operator
			// has not picked. Keep ticking so the read starts on confirm.
			return t, tick()
		}
		return t, readSnapshot(t.m.Context)
	case snapshotMsg:
		t.m.ApplySnapshot(msg.snap)
		return t, tick()
	}
	return t, nil
}

// syncLogStream reconciles the live stream with the model after a key event:
// start it when the panel just opened on a streamable service (deploy socket)
// or a CI run-log (file tail), stop it when the panel just closed. Returns a
// tea.Cmd that pumps lines, or nil.
func (t *teaModel) syncLogStream(wasOpen bool, prevSvc string, wasCI bool, wasVM bool) tea.Cmd {
	if t.m.LogsOpen && t.m.LogsVM && (!wasOpen || !wasVM) {
		t.stopLogs()
		return t.startVMLogs()
	}
	if t.m.LogsOpen && !t.m.LogsVM && t.m.LogsCI && (!wasOpen || !wasCI) {
		t.stopLogs()
		return t.startCILogs()
	}
	if t.m.LogsOpen && !t.m.LogsVM && t.m.LogsService != "" && (!wasOpen || prevSvc != t.m.LogsService) {
		t.stopLogs()
		return t.startLogs()
	}
	if !t.m.LogsOpen && wasOpen {
		t.stopLogs()
	}
	return nil
}

// startVMLogs tails the guest agent's own log through the VMLogStream seam,
// pumping lines into the panel exactly as the other two sources do.
func (t *teaModel) startVMLogs() tea.Cmd {
	if VMLogStream == nil {
		t.m.LogsErr = "vm log tailing unavailable in this build"
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.logCancel = cancel
	lines := make(chan string, 256)
	errs := make(chan string, 1)
	go func() {
		err := VMLogStream(ctx, func(s string) {
			select {
			case lines <- s:
			case <-ctx.Done():
			}
		})
		if err != nil && ctx.Err() == nil {
			select {
			case errs <- err.Error():
			default:
			}
		}
		close(lines)
	}()
	return waitLogLine(lines, errs)
}

// startCILogs tails the selected space's repo's CI run-log file via the
// CILogStream seam, pumping lines into the panel the same way startLogs does.
// The resolved file path is surfaced as the first line so the operator knows
// the source.
func (t *teaModel) startCILogs() tea.Cmd {
	if CILogStream == nil {
		t.m.LogsErr = "CI log tailing unavailable in this build"
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.logCancel = cancel
	lines := make(chan string, 256)
	errs := make(chan string, 1)
	repo, task := t.m.LogsRepo, t.m.LogsTask
	go func() {
		path, err := CILogStream(ctx, repo, task, func(s string) {
			select {
			case lines <- s:
			case <-ctx.Done():
			}
		})
		if err != nil && ctx.Err() == nil {
			select {
			case errs <- err.Error():
			default:
			}
		}
		_ = path
		close(lines)
	}()
	return waitLogLine(lines, errs)
}

// startLogs launches the follow stream in a goroutine and returns a Cmd that
// blocks for the first line; subsequent lines re-arm via waitLogLine.
func (t *teaModel) startLogs() tea.Cmd {
	if LogStream == nil {
		t.m.LogsErr = "log streaming unavailable in this build"
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.logCancel = cancel
	lines := make(chan string, 256)
	errs := make(chan string, 1)
	node, space, svc := t.m.LogsNode, t.m.LogsSpace, t.m.LogsService
	go func() {
		err := LogStream(ctx, node, space, svc, func(s string) {
			select {
			case lines <- s:
			case <-ctx.Done():
			}
		})
		if err != nil && ctx.Err() == nil {
			select {
			case errs <- err.Error():
			default:
			}
		}
		close(lines)
	}()
	return waitLogLine(lines, errs)
}

// waitLogLine blocks until the next line/err, delivering it as a msg and
// re-arming itself so the stream keeps pumping into the UI goroutine.
func waitLogLine(lines <-chan string, errs <-chan string) tea.Cmd {
	return func() tea.Msg {
		select {
		case e := <-errs:
			return logErrMsg{err: e}
		case s, ok := <-lines:
			if !ok {
				return nil
			}
			// Take everything already buffered along with this line, so a burst
			// costs ONE redraw instead of one per line.
			batch := []string{s}
			for len(batch) < logBatchCap {
				select {
				case next, ok := <-lines:
					if !ok {
						return seqMsg{msg: logLineMsg{lines: batch}, next: nil}
					}
					batch = append(batch, next)
					continue
				default:
				}
				break
			}
			return seqMsg{msg: logLineMsg{lines: batch}, next: waitLogLine(lines, errs)}
		}
	}
}

// typeIntoSearch feeds a keypress to the search prompt, reporting whether it was
// consumed there. Enter and Esc are NOT consumed: they commit and cancel the
// prompt, which Model.Update owns.
func (t *teaModel) typeIntoSearch(msg tea.KeyMsg) bool {
	switch msg.Type {
	case tea.KeyRunes:
		for _, r := range msg.Runes {
			t.m.SearchInput(r)
		}
		return true
	case tea.KeySpace:
		t.m.SearchInput(' ')
		return true
	case tea.KeyBackspace:
		t.m.SearchBackspace()
		return true
	}
	return false
}

// typeIntoJump feeds a keypress to the jump palette, reporting whether it was
// consumed there. Enter, Esc, Up/Down and Ctrl+K are NOT consumed: they
// confirm, close, move the highlight or re-close the palette, which
// Model.Update owns.
func (t *teaModel) typeIntoJump(msg tea.KeyMsg) bool {
	switch msg.Type {
	case tea.KeyRunes:
		for _, r := range msg.Runes {
			t.m.JumpInput(r)
		}
		return true
	case tea.KeySpace:
		t.m.JumpInput(' ')
		return true
	case tea.KeyBackspace:
		t.m.JumpBackspace()
		return true
	}
	return false
}

// restartDoneMsg reports the exited dev supervisor back into the restored cockpit.
type restartDoneMsg struct {
	space string
	err   string
}

// execRestart hands the terminal to `sklp dev` for as long as the supervisor
// runs, through the same tea.Exec path the shell takes.
func execRestart(req RestartRequest) tea.Cmd {
	return tea.Exec(&restartCommand{req: req}, func(err error) tea.Msg {
		msg := restartDoneMsg{space: req.Space}
		if err != nil {
			msg.err = err.Error()
		}
		return msg
	})
}

// restartCommand adapts the RestartDev seam to tea.ExecCommand. The Set* hooks
// are no-ops for the same reason shellCommand's are: the seam runs the
// supervisor on the real os.Stdin/os.Stdout, which bubbletea has released.
type restartCommand struct{ req RestartRequest }

func (c *restartCommand) Run() error {
	if RestartDev == nil {
		return errRestartUnavailable
	}
	return RestartDev(c.req.Space, c.req.Repo)
}

func (c *restartCommand) SetStdin(io.Reader)  {}
func (c *restartCommand) SetStdout(io.Writer) {}
func (c *restartCommand) SetStderr(io.Writer) {}

// errRestartUnavailable is returned when the restart seam was never injected.
var errRestartUnavailable = errors.New("restart unavailable in this build")

// execDoneMsg reports the finished shell back into the restored cockpit.
type execDoneMsg struct {
	label string
	err   string
}

// execShell suspends the cockpit and runs the interactive shell in its place,
// through tea.Exec: it releases the terminal (leaves the alt-screen, restores
// cooked mode), blocks on Run, then puts the cockpit back exactly as it was.
func execShell(req ExecRequest) tea.Cmd {
	return tea.Exec(&shellCommand{req: req}, func(err error) tea.Msg {
		msg := execDoneMsg{label: req.Label}
		if err != nil {
			msg.err = err.Error()
		}
		return msg
	})
}

// shellCommand adapts the SpaceExec seam to tea.ExecCommand. The Set*
// hooks are no-ops on purpose: the seam runs an in-process runc exec that wires
// the container PTY straight to the real os.Stdin/os.Stdout, so there is nothing
// to redirect — bubbletea has already released them by the time Run is called.
type shellCommand struct{ req ExecRequest }

func (c *shellCommand) Run() error {
	if SpaceExec == nil {
		return errExecUnavailable
	}
	return SpaceExec(c.req.Node, c.req.Space, c.req.Container)
}

func (c *shellCommand) SetStdin(io.Reader)  {}
func (c *shellCommand) SetStdout(io.Writer) {}
func (c *shellCommand) SetStderr(io.Writer) {}

// errExecUnavailable is returned when the exec seam was never injected (a build
// without the embedded runtime).
var errExecUnavailable = errors.New("exec unavailable in this build")

// seqMsg lets a Cmd deliver a msg AND schedule the next Cmd in one step.
type seqMsg struct {
	msg  tea.Msg
	next tea.Cmd
}

// stopLogs cancels the active follow stream, if any.
func (t *teaModel) stopLogs() {
	if t.logCancel != nil {
		t.logCancel()
		t.logCancel = nil
	}
}

func (t *teaModel) View() string { return Render(t.m, t.r) }

// mapKey maps a native tea key event onto our library-agnostic Key.
func mapKey(msg tea.KeyMsg) Key {
	switch msg.Type {
	case tea.KeyUp:
		return KeyUp
	case tea.KeyDown:
		return KeyDown
	case tea.KeyTab:
		return KeyTab
	case tea.KeyEnter:
		return KeyEnter
	case tea.KeyEsc:
		return KeyBack
	case tea.KeyCtrlC:
		return KeyQuit
	case tea.KeyCtrlK:
		return KeyJump
	case tea.KeyPgUp:
		return KeyPageUp
	case tea.KeyPgDown:
		return KeyPageDown
	case tea.KeyHome:
		return KeyHome
	case tea.KeyEnd:
		return KeyEnd
	}
	// Case-sensitive: g and G are opposite ends of the buffer.
	switch msg.String() {
	case "g":
		return KeyHome
	case "G":
		return KeyEnd
	}
	// Case-sensitive: d and D are different actions (delete one vs purge all),
	// and C (context picker) must not collide with c (copy the log buffer).
	switch msg.String() {
	case "D":
		return KeyPurge
	case "C":
		return KeyContext
	case "V":
		return KeyVMProcs
	}
	switch strings.ToLower(msg.String()) {
	case "q":
		return KeyQuit
	case "f":
		return KeyFilter
	case "s":
		return KeyStop
	case "o":
		return KeySort
	case "l":
		return KeyLogs
	case "v":
		return KeyVMLogs
	case "c":
		return KeyCopy
	case "e":
		return KeyExec
	case "k":
		return KeyUp
	case "j":
		return KeyDown
	case "d":
		return KeyKill
	case "r":
		return KeyRestart
	case "y":
		return KeyConfirm
	case "n":
		return KeyBack // cancel a pending confirm
	}
	return KeyNone
}

// lipRenderer is the lipgloss-backed Renderer.
type lipRenderer struct{}

// NewLipglossRenderer returns the default styled Renderer.
func NewLipglossRenderer() Renderer { return lipRenderer{} }

// The palette must NOT depend on the terminal's background: lipgloss detects it
// via an OSC query that bubbletea's raw mode + alt-screen leaves unanswered, so
// the answer flips between runs and a background-specific palette ends up
// painting dark-on-dark or pale-on-white.
//
// So nothing here sets a plain foreground or background for body text. Ordinary
// rows inherit the terminal's own colors — legible by construction, whatever the
// theme — and hierarchy comes from weight and from reverse video, which swaps
// whatever those colors are. Only the accents are colored, and each is picked at
// mid-luminance so it reads on white and on black alike.
var (
	colAccent = lipgloss.CompleteColor{TrueColor: "#7D56F4", ANSI256: "99", ANSI: "5"}
	colOK     = lipgloss.CompleteColor{TrueColor: "#2FA85A", ANSI256: "35", ANSI: "2"}
	colWarn   = lipgloss.CompleteColor{TrueColor: "#B8860B", ANSI256: "136", ANSI: "3"}
	colErr    = lipgloss.CompleteColor{TrueColor: "#D0455A", ANSI256: "167", ANSI: "1"}
	colDim    = lipgloss.CompleteColor{TrueColor: "#8A8F98", ANSI256: "245", ANSI: "8"}
	colCrumbF = lipgloss.CompleteColor{TrueColor: "#FFFFFF", ANSI256: "231", ANSI: "7"}
)

var (
	styBox      = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colDim).Padding(0, 1)
	styTitle    = lipgloss.NewStyle().Bold(true)
	styMuted    = lipgloss.NewStyle().Foreground(colDim)
	styOK       = lipgloss.NewStyle().Foreground(colOK)
	styWarn     = lipgloss.NewStyle().Foreground(colWarn)
	styErr      = lipgloss.NewStyle().Foreground(colErr)
	styHdr      = lipgloss.NewStyle().Bold(true).Foreground(colDim)
	styRow      = lipgloss.NewStyle()
	styRowSel   = lipgloss.NewStyle().Bold(true).Reverse(true)
	styCrumb    = lipgloss.NewStyle().Bold(true).Foreground(colCrumbF).Background(colAccent).Padding(0, 1)
	styCrumbEnd = lipgloss.NewStyle().Foreground(colDim)
	styKeyCap   = lipgloss.NewStyle().Foreground(colAccent)
	styLabel    = lipgloss.NewStyle().Foreground(colDim)
	styValue    = lipgloss.NewStyle().Bold(true)
	styMark     = lipgloss.NewStyle().Foreground(colAccent)
	// Reverse video, not a foreground: the log text it marks is already coloured,
	// so only swapping fore/background reliably stands out on any theme.
	styMatch = lipgloss.NewStyle().Reverse(true).Bold(true)
)

// boxInner is the content width for a total box width. lipgloss' Width() is the
// padded content box, so the border (2 cols) is the only thing to subtract.
func boxInner(width int) int {
	inner := width - 2
	if inner < 1 {
		return 1
	}
	return inner
}

func (lipRenderer) Box(title, body string, width int) string {
	return styBox.Width(boxInner(width)).Render(styTitle.Render(title) + "\n" + body)
}

// Table renders at the exact widths the caller computed, and paints every line
// across the FULL width: a selection whose background stops at the last column
// reads as a stray block rather than as the current row.
func (lipRenderer) Table(cols []Column, rows []Row, width int) string {
	var b strings.Builder
	headers := make([]string, len(cols))
	widths := make([]int, len(cols))
	for i, c := range cols {
		headers[i] = c.Header
		widths[i] = c.Width
	}
	// The line is built at the full width up front rather than left to Width(),
	// which pads a styled block AFTER rendering and leaves the background
	// restarting past a visible gap.
	indent := strings.Repeat(" ", tableIndent)
	cellsW := width - tableIndent
	b.WriteString(styHdr.Render(indent + joinCells(headers, widths, cellsW)))
	b.WriteString("\n")
	for _, row := range rows {
		// Only an error claims the whole row: a failure must be impossible to
		// scroll past. Idle or busy tint the row's text down to unreadable for a
		// nuance the glyph already carries, so they keep the normal foreground.
		st := styRow
		if row.State == StateError {
			st = styErr
		}
		if row.Selected {
			st = st.Reverse(true).Bold(true)
		}
		// The row is rendered as ONE span so its background runs unbroken, then
		// the leading glyph is recolored in place: styling it as a cell would end
		// its color with a reset, cutting the row's background mid-line.
		line := st.Render(indent + joinCells(row.Cells, widths, cellsW))
		b.WriteString(colorizeGlyph(line, st, row.State))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// colorizeGlyph tints the row's leading status mark without interrupting the
// row's own span: it re-opens that span right after the mark, so the selection
// background keeps running to the end of the line.
func colorizeGlyph(line string, rowStyle lipgloss.Style, state State) string {
	color, ok := glyphColor(state)
	if !ok {
		return line
	}
	glyph := lipRenderer{}.StatusGlyph(state)
	i := strings.Index(line, glyph)
	if i < 0 {
		return line
	}
	tinted := rowStyle.Foreground(color).Render(glyph)
	rest := rowStyle.Render(line[i+len(glyph):])
	return line[:i] + tinted + rest
}

// glyphColor is the accent a status mark carries. StateReady has none: a healthy
// row needs no emphasis, and green-on-selection reads as an error state.
func glyphColor(state State) (lipgloss.TerminalColor, bool) {
	switch state {
	case StateReady:
		return colOK, true
	case StateBusy:
		return colWarn, true
	case StateError:
		return colErr, true
	case StateIdle, StateNone:
		return colDim, true
	}
	return nil, false
}

// cellGap is the column separator width used by joinCells.
const cellGap = 2

// joinCells lays the cells out at their exact widths and pads the result to
// total. It must NOT trim its trailing padding: a row is painted as one styled
// block, so a short line would leave the background restarting after a gap
// instead of running the full width.
func joinCells(cells []string, widths []int, total int) string {
	var b strings.Builder
	for i, c := range cells {
		w := 0
		if i < len(widths) {
			w = widths[i]
		}
		c = truncate(c, w)
		b.WriteString(c)
		if pad := w - lipgloss.Width(c); pad > 0 {
			b.WriteString(strings.Repeat(" ", pad))
		}
		b.WriteString(strings.Repeat(" ", cellGap))
	}
	line := b.String()
	if pad := total - lipgloss.Width(line); pad > 0 {
		line += strings.Repeat(" ", pad)
	}
	return line
}

// truncate shortens s to width columns, marking the cut with an ellipsis so a
// clipped value never reads as a complete one.
func truncate(s string, width int) string {
	if width <= 0 || lipgloss.Width(s) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r))+1 > width {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

func (lipRenderer) Header(fields [][2]string, keys [][2]string, width int) string {
	labelW := 0
	for _, f := range fields {
		if w := lipgloss.Width(f[0]); w > labelW {
			labelW = w
		}
	}
	// A header field that wraps pushes every following line out of the frame, so
	// values are clipped to the space the frame actually has.
	content := boxInner(width) - boxPadding
	valueW := content - labelW - 1
	left := make([]string, len(fields))
	for i, f := range fields {
		left[i] = styLabel.Render(f[0]+strings.Repeat(" ", labelW-lipgloss.Width(f[0]))) + " " + styValue.Render(truncate(f[1], valueW))
	}
	cols := []string{lipgloss.JoinVertical(lipgloss.Left, left...)}

	if len(keys) > 0 {
		keyW, descW := 0, 0
		for _, k := range keys {
			if w := lipgloss.Width(k[0]); w > keyW {
				keyW = w
			}
			if w := lipgloss.Width(k[1]); w > descW {
				descW = w
			}
		}
		// Only claim the key column when it fits whole: a wrapped hint is worse
		// than no hint, and the footer repeats the bindings anyway.
		if lipgloss.Width(cols[0])+headerColGap+keyW+1+descW <= content {
			lines := make([]string, len(keys))
			for i, k := range keys {
				lines[i] = styKeyCap.Render(k[0]+strings.Repeat(" ", keyW-lipgloss.Width(k[0]))) + " " + styMuted.Render(k[1])
			}
			cols = append(cols, strings.Repeat(" ", headerColGap), lipgloss.JoinVertical(lipgloss.Left, lines...))
		}
	}

	body := lipgloss.JoinHorizontal(lipgloss.Top, cols...)
	if width >= headerMarkMinWidth {
		body = appendMark(body, content)
	}
	return styBox.Width(boxInner(width)).Render(body)
}

// appendMark right-aligns the product mark onto the header's first lines. It
// pads line by line rather than going through JoinHorizontal, which re-measures
// the block and would push the mark past the frame.
func appendMark(body string, content int) string {
	markW := 0
	for _, l := range productMark {
		if w := lipgloss.Width(l); w > markW {
			markW = w
		}
	}
	lines := strings.Split(body, "\n")
	bodyW := 0
	for _, l := range lines {
		if w := lipgloss.Width(l); w > bodyW {
			bodyW = w
		}
	}
	if bodyW+headerColGap+markW > content || len(lines) < len(productMark) {
		return body
	}
	pad := content - markW
	for i, mark := range productMark {
		lines[i] += strings.Repeat(" ", pad-lipgloss.Width(lines[i])) + styMark.Render(mark)
	}
	return strings.Join(lines, "\n")
}

// headerMarkMinWidth is the width below which the header drops its product mark;
// the key column is dropped dynamically when it would not fit whole.
const (
	headerMarkMinWidth = 100
	headerColGap       = 3
	boxPadding         = 2
)

// productMark is deliberately drawn from box-drawing characters, which are
// unambiguously single-width: the half-block glyphs of a denser logo are East
// Asian Ambiguous, so terminals disagree on their width and the header wraps.
var productMark = []string{
	"┌─┐┬┌─┌─┐┬  ┌─┐┌─┐┬",
	"└─┐├┴┐├─┤│  ├─┘├─┤│",
	"└─┘┴ ┴┴ ┴┴─┘┴  ┴ ┴┴",
	"     c o c k p i t ",
}

func (lipRenderer) Crumb(path, right string, width int) string {
	left := styCrumb.Render(path)
	gap := width - lipgloss.Width(left) - lipgloss.Width(right) - 1
	if gap < 1 {
		return left
	}
	return left + strings.Repeat(" ", gap) + styCrumbEnd.Render(right)
}

// StatusGlyph returns the mark UNSTYLED. Inside a table the row owns the color
// for its whole span; a self-colored glyph would close that span with a reset
// and cut the selection background mid-line.
func (lipRenderer) StatusGlyph(state State) string {
	switch state {
	case StateReady:
		return "●"
	case StateBusy:
		return "◐"
	case StateIdle:
		return "○"
	case StateError:
		return "✗"
	default:
		return "·"
	}
}

func (lipRenderer) Text(s string, style Style) string {
	switch style {
	case StyleTitle:
		return styTitle.Render(s)
	case StyleMuted:
		return styMuted.Render(s)
	case StyleOK:
		return styOK.Render(s)
	case StyleWarn:
		return styWarn.Render(s)
	case StyleErr:
		return styErr.Render(s)
	case StyleMatch:
		return styMatch.Render(s)
	default:
		return s
	}
}

func (lipRenderer) HRule(width int) string {
	return styMuted.Render(strings.Repeat("─", width))
}

// SideBySide joins two blocks horizontally, top-aligned. Every left line is
// normalised to exactly leftWidth — padded when short, clamped when long.
// JoinHorizontal aligns the right column on the WIDEST left line, so a single
// overflowing line (a long volume list, an error) would otherwise push the logs
// pane off-screen and collapse it to a few columns.
func (lipRenderer) SideBySide(left, right string, leftWidth int) string {
	clamp := lipgloss.NewStyle().MaxWidth(leftWidth)
	lines := strings.Split(left, "\n")
	for i, l := range lines {
		if lipgloss.Width(l) > leftWidth {
			l = clamp.Render(l)
		}
		if pad := leftWidth - lipgloss.Width(l); pad > 0 {
			l += strings.Repeat(" ", pad)
		}
		lines[i] = l
	}
	gutter := strings.Repeat(" ", splitGutter)
	return lipgloss.JoinHorizontal(lipgloss.Top, strings.Join(lines, "\n"), gutter, right)
}
