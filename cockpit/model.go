package cockpit

import "strings"

// Filter buckets the per-space service view. v1 has no reliable dev/pipe/deploy
// classification in the control StateResponse, so non-All filters are honest
// hints, not fabricated categories (see model.FilterHint / state.go).
type Filter int

const (
	FilterAll Filter = iota
	FilterDev
	FilterPipe
	FilterDeploy
)

func (f Filter) String() string {
	switch f {
	case FilterDev:
		return "dev"
	case FilterPipe:
		return "pipe"
	case FilterDeploy:
		return "deploy"
	default:
		return "all"
	}
}

// Sort orders the spaces list.
type Sort int

const (
	SortName Sort = iota
	SortState
)

func (s Sort) String() string {
	if s == SortState {
		return "state"
	}
	return "name"
}

// Milestone is a single flow lifecycle step's status.
type Milestone int

const (
	MilestoneUnknown Milestone = iota // could not be determined; degrade gracefully
	MilestoneTodo                     // not reached yet
	MilestoneActive                   // currently in progress
	MilestoneDone                     // reached
)

// FlowState is the read-only snapshot of the current feature lifecycle. Empty
// Branch means no active flow (collapsed banner).
type FlowState struct {
	Branch   string // current branch, "" when undetectable
	Onto     string // target ref the flow builds on
	Worktree string // worktree path, "" when in-place / unknown
	Changed  int    // files changed vs Onto

	Start  Milestone // branch opened
	Coding Milestone // files changed
	CI     Milestone // a CI run active (best-effort)
	PR     Milestone // a PR open (best-effort; often Unknown)
}

// Active reports whether a flow banner should be shown expanded.
func (f FlowState) Active() bool { return f.Branch != "" }

// Service is one service row inside a space (control.ServiceState, flattened).
type Service struct {
	Name      string
	State     State
	Instances int
	// Container is the runc id to exec into, set for rows derived from the runc
	// state (non-supervised spaces). Empty on a supervised row, where the exec
	// target is resolved from the service name by the registry.
	Container string
}

// Space is one space (running or stopped) with its services when supervised.
type Space struct {
	Name string
	// Node names the machine this space runs on: "" for the local host, else
	// the context name of the remote node it was read from. Spaces from
	// different nodes can share a name, so it is part of the row's identity —
	// selection and actions must carry it, never the name alone.
	Node     string
	AllReady bool
	Services []Service
	// Status is the runc-state status (running|stopped|"") for non-supervised rows.
	Status string
	// Containers is the runc container count, shown for non-supervised spaces.
	Containers int
	// Volumes names this space's named volumes. Teardown destroys them with the
	// space, so the confirm prompt can state what data is about to go.
	Volumes []string
	// Supervised is true when a deploy control socket answered for this space.
	Supervised bool
	// Repo is the host repo path bind-mounted at /workspace (from the bundle).
	// Used to tail this space's repo's central CI log when non-supervised.
	Repo string
	// LimitedInfo flags a non-supervised space: no control API, no services list.
	LimitedInfo bool
	// Err carries a per-space read failure (socket died mid-scan); the space is
	// still listed so the view is honest about partial data.
	Err string
}

// HostRollup is the aggregate count across all spaces.
type HostRollup struct {
	Spaces     int // total spaces (matches `space ls`)
	Supervised int // spaces with a live control API
	Services   int
	Ready      int
	Busy       int
	Errored    int
}

// Level is the drill-in focus: SPACES selects a space, SERVICES picks a service
// inside the selected supervised space.
type Level int

const (
	LevelSpaces   Level = iota // ↑/↓ select a space (default)
	LevelServices              // ↑/↓ pick a service inside the entered space
)

// logBufferCap bounds the in-memory log ring buffer for the panel.
const logBufferCap = 1000

// Model is the pure cockpit state. It carries NO TUI types and is mutated only
// by Update (key events) and by state.go (real-source reads). Rendering is
// derived from it in view.go.
type Model struct {
	Flow   FlowState
	Spaces []Space
	Host   HostRollup
	// VM is what the guest can tell about the machine the cockpit runs on.
	// Zero-valued where nothing could be read; see VMState.Known.
	VM VMState

	// HostRuns are the host-only pipelines (space:false, e.g. publish) — shown in
	// their own panel since they create no space to appear in the Spaces list.
	HostRuns []HostRun
	Selected int
	Filter   Filter
	Sort     Sort
	// Level is the drill-in focus; SvcIndex is the highlighted service at LevelServices.
	Level    Level
	SvcIndex int
	// Note is a transient hint (e.g. failed drill-in), surfaced by the view.
	Note string
	// PendingKill is the space name awaiting kill confirmation; "" when none.
	PendingKill string
	// PendingPurge arms the bulk teardown of every idle space; false when none.
	PendingPurge bool
	// Err is a top-level load failure (e.g. ListSpaces failed) shown as a banner.
	Err string

	// PendingExec is the shell the operator asked for, consumed by the TUI layer:
	// the model is pure (no I/O), and an interactive shell needs the terminal
	// itself, so Update can only record the request. Nil when none is armed.
	PendingExec *ExecRequest

	// Width/Height are the last known terminal size (pure data, set from the TUI layer); 0 means unknown.
	Width  int
	Height int

	// LogsOpen toggles the right-half live logs panel.
	LogsOpen bool
	// LogsService / LogsSpace identify the streamed service while the panel is open.
	LogsService string
	LogsSpace   string
	// LogsNode is the machine the streamed space runs on ("" = local). The seam
	// needs it to pick a transport: the local socket, or that node's control API.
	LogsNode string
	// LogsLines is the bounded log ring buffer (cap logBufferCap).
	LogsLines []string
	// LogsScroll is the top visible line index; -1 means follow (pinned to tail).
	LogsScroll int
	// LogsErr carries an honest panel-level message (e.g. logs unavailable).
	LogsErr string
	// LogsVM marks the logs panel as tailing the guest agent's own record
	// rather than a space or a run.
	LogsVM bool

	// LogsCI marks the panel as tailing a CI run-log file (no control socket)
	// rather than streaming a supervised service over the deploy API.
	LogsCI bool
	// LogsRepo is the host repo path whose central CI log the panel tails when
	// LogsCI is set (recovered from the selected space's bundle; "" → cwd).
	LogsRepo string
	// LogsTask is the CI task whose log the panel tails (the space name). Keys
	// the per-task log file ci-<task>-<ts>.log so concurrent pipelines in one
	// repo don't collide.
	LogsTask string
	// LogsPath is the run-log file the panel resolved to, set by the seam once
	// tailing starts. It decides whether the panel titles itself a CI or a dev
	// run-log — the space alone cannot tell, both land in the same store.
	LogsPath string
	// LogsNote is the transient result of a panel action ([c] copy), shown in the
	// panel footer. Note lives in the cockpit column, which the full-screen logs
	// pane replaces.
	LogsNote string

	// SearchTyping is true while the operator composes a query in the logs panel:
	// the TUI routes printable keys to SearchInput instead of the normal bindings,
	// so a query may contain g, c, q… without triggering their action.
	SearchTyping bool
	// SearchQuery is the active needle, matched case-insensitively against the
	// UNWRAPPED buffer lines. Empty means no search.
	SearchQuery string
	// SearchMatch indexes the currently focused hit in the match list, -1 when the
	// query matches nothing.
	SearchMatch int

	// PendingRestart is the dev stack the operator asked to relaunch, consumed by
	// the TUI layer: relaunching runs a foreground supervisor that needs the
	// terminal, which the pure model cannot hand over. Nil when none is armed.
	PendingRestart *RestartRequest

	// JumpOpen toggles the Ctrl+K jump palette: a fuzzy filter over every space
	// and container/service name, so the operator reaches one across nodes
	// without paging through the grouped table.
	JumpOpen bool
	// JumpQuery is the query typed into the palette.
	JumpQuery string
	// JumpIndex highlights one result in the filtered list, -1 when the query
	// matches nothing.
	JumpIndex int

	// Context is the node the cockpit is pointed at: "" (LocalContext) is this
	// machine, otherwise a configured deploy context. Only this node is read.
	Context string
	// ContextChosen records that the operator has picked a context at least
	// once. Until then the picker cannot be dismissed — there is nothing to go
	// back to.
	ContextChosen bool
	// ContextsOpen toggles the context picker overlay.
	ContextsOpen bool
	// ContextChoices is the list the picker draws, built when it opens so a
	// context added to the config is picked up without restarting.
	ContextChoices []ContextChoice
	// ContextIndex highlights one row of the picker.
	ContextIndex int

	// VMProcsOpen expands the VM panel's process list. Collapsed by default:
	// the list is the machine's top consumers, which on a workstation is the
	// browser rather than anything the cockpit manages, and it costs half the
	// screen. The summary line above it stays visible either way.
	VMProcsOpen bool
}

// Selection returns the selected space, or nil when the selection falls on the
// host-runs section (indices >= len(Spaces)) or the list is empty. The combined
// list is [spaces…, host-runs…]; space-scoped actions are no-ops on a host-run.
func (m *Model) Selection() *Space {
	if m.Selected < 0 || m.Selected >= len(m.Spaces) {
		return nil
	}
	return &m.Spaces[m.Selected]
}

// SelectedHostRun returns the highlighted host-run, or nil when the selection is
// on a space. Host-runs occupy indices [len(Spaces), len(Spaces)+len(HostRuns)).
func (m *Model) SelectedHostRun() *HostRun {
	i := m.Selected - len(m.Spaces)
	if i < 0 || i >= len(m.HostRuns) {
		return nil
	}
	return &m.HostRuns[i]
}

// rowCount is the total selectable rows: spaces plus host-runs.
func (m *Model) rowCount() int { return len(m.Spaces) + len(m.HostRuns) }

// SelectedService returns the highlighted service at LevelServices, or nil.
func (m *Model) SelectedService() *Service {
	if m.Level != LevelServices {
		return nil
	}
	sp := m.Selection()
	if sp == nil || m.SvcIndex < 0 || m.SvcIndex >= len(sp.Services) {
		return nil
	}
	return &sp.Services[m.SvcIndex]
}

// EnterSpace drills into the selected space's service list. A non-supervised
// space is NOT a dead end: its rows come from the runc state (container name +
// status), which is enough to see what runs and to exec into it — only readiness
// and captured logs need a control API. An empty space has nothing to enter.
func (m *Model) EnterSpace() bool {
	sp := m.Selection()
	if sp == nil {
		return false
	}
	if len(sp.Services) == 0 {
		m.Note = "no containers to drill into (" + sp.Name + " is empty)"
		return false
	}
	m.Level = LevelServices
	m.SvcIndex = 0
	m.Note = ""
	return true
}

// ExecRequest names the container a pending shell should open in.
type ExecRequest struct {
	Space string
	// Node is the machine the space runs on ("" = local, reached over the Unix
	// socket; otherwise the configured node, reached over its control API).
	Node string
	// Container is the runc id when the row carries one (a non-supervised space's
	// container row), else the service name for the registry to resolve.
	Container string
	// Label is how the target is named back to the operator.
	Label string
}

// requestExec arms a shell in the highlighted container. Only at LevelServices
// with the logs panel closed: the panel owns the screen, and at LevelSpaces a
// space with several containers gives no unambiguous target — the operator drills
// in first and picks one.
func (m *Model) requestExec() bool {
	if m.LogsOpen {
		return false
	}
	if m.Level != LevelServices {
		m.Note = "enter a space first (↵), then [e] opens a shell in the highlighted container"
		return true
	}
	sp, svc := m.Selection(), m.SelectedService()
	if sp == nil || svc == nil {
		return false
	}
	if svc.State != StateReady && svc.State != StateBusy {
		m.Note = svc.Name + " is not running — nothing to exec into"
		return true
	}
	if SpaceExec == nil {
		m.Note = "exec unavailable in this build"
		return true
	}
	// A remote target is addressed by SERVICE name: the control API resolves the
	// service to a container on its own machine, and a runc id read from a local
	// row would name nothing there.
	target := svc.Name
	if sp.Node == "" && svc.Container != "" {
		target = svc.Container
	}
	m.PendingExec = &ExecRequest{Space: sp.Name, Node: sp.Node, Container: target, Label: svc.Name}
	m.Note = ""
	return true
}

// TakePendingExec hands the armed request to the TUI layer and disarms it, so a
// shell is opened exactly once per keypress.
func (m *Model) TakePendingExec() *ExecRequest {
	req := m.PendingExec
	m.PendingExec = nil
	return req
}

// BackLevel returns to LevelSpaces, clearing the service highlight.
func (m *Model) BackLevel() bool {
	if m.Level == LevelSpaces {
		return false
	}
	m.Level = LevelSpaces
	m.SvcIndex = 0
	m.Note = ""
	return true
}

// moveService shifts the service highlight, clamped to [0, len-1]. No wrap.
func (m *Model) moveService(delta int) bool {
	sp := m.Selection()
	if sp == nil || len(sp.Services) == 0 {
		return false
	}
	next := m.SvcIndex + delta
	if next < 0 {
		next = 0
	}
	if next > len(sp.Services)-1 {
		next = len(sp.Services) - 1
	}
	if next == m.SvcIndex {
		return false
	}
	m.SvcIndex = next
	return true
}

// requestKill arms the kill confirmation for the selected space. Only at
// LevelSpaces with the panel closed; supervised spaces are deploys and are
// refused outright (no confirm) per the space/deploy frontier.
func (m *Model) requestKill() bool {
	if m.Level != LevelSpaces || m.LogsOpen {
		return false
	}
	sp := m.Selection()
	if sp == nil {
		return false
	}
	if note := remoteActionNote(sp, "delete"); note != "" {
		m.Note = note
		return true
	}
	if sp.Supervised {
		m.Note = sp.Name + " is a supervised deploy — use `sklp deploy delete`"
		return true
	}
	m.PendingKill = sp.Name
	m.Note = ""
	return true
}

// confirmKill performs the armed kill through the injected seam. Down DESTROYS
// the named volumes with the space — which is why the prompt names them and why
// [s] exists for a halt that keeps them. On success the space is dropped and the
// selection clamped; on error the message is surfaced. Either way the pending
// state is cleared.
func (m *Model) confirmKill() bool {
	name := m.PendingKill
	m.PendingKill = ""
	if name == "" {
		return false
	}
	if KillSpace == nil {
		m.Note = "kill unavailable in this build"
		return true
	}
	if err := KillSpace(name); err != nil {
		m.Note = "kill " + name + " failed: " + err.Error()
		return true
	}
	m.removeSpace(name)
	m.Note = "space " + name + " torn down"
	return true
}

// statusStopped is the registry's aggregate status for a space whose state exists
// but whose containers are all down (space_registry.aggregateStatus).
const statusStopped = "stopped"

// requestStop halts the selected space through the StopSpace seam, keeping its
// volumes. Unlike a kill this needs no confirmation: the data survives and `space
// up` brings the space back, so there is nothing irreversible to guard.
func (m *Model) requestStop() bool {
	if m.Level != LevelSpaces || m.LogsOpen {
		return false
	}
	sp := m.Selection()
	if sp == nil {
		return false
	}
	if note := remoteActionNote(sp, "stop"); note != "" {
		m.Note = note
		return true
	}
	if sp.Supervised {
		m.Note = sp.Name + " is a supervised deploy — use `sklp deploy stop`"
		return true
	}
	if sp.Status == statusStopped {
		m.Note = sp.Name + " is already stopped"
		return true
	}
	if StopSpace == nil {
		m.Note = "stop unavailable in this build"
		return true
	}
	if err := StopSpace(sp.Name); err != nil {
		m.Note = "stop " + sp.Name + " failed: " + err.Error()
		return true
	}
	// The row stays listed — that is the point of a stop — so reflect the new
	// state until the next refresh reads it back from the registry.
	sp.Status = statusStopped
	sp.Containers = 0
	sp.AllReady = false
	m.Note = "space " + sp.Name + " stopped (volumes kept)"
	return true
}

// spaceByName finds a listed space by name, nil when absent. The pending-kill
// prompt needs the row for a name it captured earlier, which a refresh may have
// dropped in the meantime.
func (m *Model) spaceByName(name string) *Space {
	for i := range m.Spaces {
		// Local rows only: the seams this resolves for act on the local registry.
		if m.Spaces[i].Name == name && m.Spaces[i].Node == "" {
			return &m.Spaces[i]
		}
	}
	return nil
}

// remoteActionNote returns the refusal to show when a LOCAL-only action is
// aimed at a space on another node, or "" when the space is local. The
// DESTRUCTIVE seams (kill, stop) go through the local registry, so running one
// on a remote row would tear down a same-named LOCAL space instead — the wrong
// machine's data. Exec is not among them: it routes over the node's control
// API and so addresses the machine the row actually names.
func remoteActionNote(sp *Space, verb string) string {
	if sp.Node == "" {
		return ""
	}
	return sp.Name + " runs on node " + sp.Node + " — this cockpit can only " + verb + " local spaces"
}

// localSpaceCount counts the rows the purge seam can actually reap. Remote
// rows are excluded: the seam tears down local spaces, so a list made only of
// other nodes' spaces has nothing to purge.
func (m *Model) localSpaceCount() int {
	n := 0
	for _, sp := range m.Spaces {
		if sp.Node == "" {
			n++
		}
	}
	return n
}

// idleSpaceCount is a LOWER BOUND on what a purge reaps: the summary carries a
// container count, not per-container status, so a space whose only container is
// stopped reads as busy here yet is purgeable to the registry. Used to decide
// whether a purge is worth arming and to size the prompt — never to predict the
// exact set, which only the seam's return value reports.
func (m *Model) idleSpaceCount() int {
	n := 0
	for _, sp := range m.Spaces {
		if sp.Node == "" && sp.Containers == 0 {
			n++
		}
	}
	return n
}

// idleVolumeCount is the data a purge would destroy, counted over the same rows
// idleSpaceCount covers — a lower bound, for the same reason.
func (m *Model) idleVolumeCount() int {
	n := 0
	for _, sp := range m.Spaces {
		if sp.Node == "" && sp.Containers == 0 {
			n += len(sp.Volumes)
		}
	}
	return n
}

// requestPurge arms the bulk teardown. Unlike requestKill it ignores the
// selection: the purge is defined by liveness, not by what is highlighted.
func (m *Model) requestPurge() bool {
	if m.Level != LevelSpaces || m.LogsOpen {
		return false
	}
	if m.localSpaceCount() == 0 {
		m.Note = "no space to purge"
		return true
	}
	m.PendingPurge = true
	m.Note = ""
	return true
}

// confirmPurge tears down every idle space through the injected seam. Volumes
// are never touched from the TUI, mirroring confirmKill.
func (m *Model) confirmPurge() bool {
	m.PendingPurge = false
	if PurgeSpaces == nil {
		m.Note = "purge unavailable in this build"
		return true
	}
	reaped, err := PurgeSpaces()
	if err != nil {
		m.Note = "purge failed: " + err.Error()
		return true
	}
	for _, name := range reaped {
		m.removeSpace(name)
	}
	if len(reaped) == 0 {
		m.Note = "no idle space to purge"
		return true
	}
	m.Note = "purged " + itoa(len(reaped)) + " space(s): " + joinNames(reaped)
	return true
}

// removeSpace drops the named space from the list and clamps the selection so
// the next Refresh stays consistent.
func (m *Model) removeSpace(name string) {
	out := m.Spaces[:0]
	for _, sp := range m.Spaces {
		// Local rows only: a remote node may run a space of the same name, and a
		// local teardown says nothing about it.
		if sp.Name != name || sp.Node != "" {
			out = append(out, sp)
		}
	}
	m.Spaces = out
	if m.Selected > len(m.Spaces)-1 {
		m.Selected = len(m.Spaces) - 1
	}
	if m.Selected < 0 {
		m.Selected = 0
	}
}

// FilterHint returns a non-empty honest note when the active filter cannot be
// applied because the data to classify services does not exist yet.
func (m *Model) FilterHint() string {
	if m.Filter == FilterAll {
		return ""
	}
	return "filter " + m.Filter.String() + " is a no-op: control state does not classify services yet"
}

// confirms reports whether k answers an armed prompt with yes. Enter is accepted
// alongside y: a prompt asks a question, and Enter is the reflex for answering
// one.
func confirms(k Key) bool { return k == KeyConfirm || k == KeyEnter }

// Update mutates the model on a key event. Pure: no I/O, no rendering. Returns
// whether the model changed (the App may skip a redraw otherwise).
func (m *Model) Update(k Key) bool {
	if m.ContextsOpen {
		// The picker is modal: until a context is chosen there is nothing to
		// render behind it, so it owns every key except quit.
		switch k {
		case KeyUp:
			return m.moveContext(-1)
		case KeyDown:
			return m.moveContext(+1)
		case KeyEnter, KeyConfirm:
			return m.ConfirmContext()
		case KeyBack, KeyContext:
			return m.CloseContexts()
		default:
			return false
		}
	}
	if k == KeyContext {
		return m.OpenContexts()
	}
	if k == KeyVMProcs {
		m.VMProcsOpen = !m.VMProcsOpen
		return true
	}
	if m.JumpOpen {
		// The palette owns every printable key (the TUI routes those to
		// JumpInput), so only the control keys that move, confirm or close it
		// reach here.
		switch k {
		case KeyUp:
			return m.moveJump(-1)
		case KeyDown:
			return m.moveJump(+1)
		case KeyEnter:
			return m.ConfirmJump()
		case KeyBack, KeyJump:
			return m.CloseJump()
		default:
			return false
		}
	}
	if k == KeyJump {
		return m.OpenJump()
	}
	if m.LogsOpen {
		// A search prompt owns every printable key (the TUI routes those to
		// SearchInput), so only the control keys that close or commit it reach here.
		if m.SearchTyping {
			switch k {
			case KeyEnter:
				return m.CommitSearch()
			case KeyBack:
				return m.ClearSearch()
			default:
				return false
			}
		}
		switch k {
		case KeySearch:
			return m.StartSearch()
		case KeyNext:
			return m.NextMatch()
		case KeyPrev:
			return m.PrevMatch()
		case KeyBack:
			// Esc drops an active search before it closes the panel, so leaving a
			// search does not also lose the buffer the operator was reading.
			if m.SearchQuery != "" {
				return m.ClearSearch()
			}
			return false
		case KeyUp:
			return m.ScrollLogs(-m.logsStep())
		case KeyDown:
			return m.ScrollLogs(+m.logsStep())
		case KeyPageUp:
			return m.ScrollLogs(-m.logsPage())
		case KeyPageDown:
			return m.ScrollLogs(+m.logsPage())
		case KeyHome:
			return m.ScrollLogsToTop()
		case KeyEnd:
			return m.FollowLogs()
		case KeyCopy:
			return m.CopyLogs()
		case KeyVMLogs:
			m.ToggleVMLogs()
			return true
		case KeyLogs:
			m.CloseLogs()
			return true
		default:
			return false
		}
	}
	// A pending confirm intercepts the next key: y or Enter confirms, anything
	// else (n/Esc/navigation) cancels before the key is otherwise handled. Enter
	// counts because a prompt is what the operator is answering — reaching for it
	// and silently cancelling is worse than either outcome.
	if m.PendingKill != "" {
		if confirms(k) {
			return m.confirmKill()
		}
		m.PendingKill = ""
		m.Note = "kill cancelled"
		if k == KeyBack || k == KeyKill {
			return true
		}
		// fall through: let the cancelling navigation key act normally.
	}
	if m.PendingPurge {
		if confirms(k) {
			return m.confirmPurge()
		}
		m.PendingPurge = false
		m.Note = "purge cancelled"
		if k == KeyBack || k == KeyPurge {
			return true
		}
	}
	switch k {
	case KeyRestart:
		return m.requestRestart()
	case KeyKill:
		return m.requestKill()
	case KeyPurge:
		return m.requestPurge()
	case KeyStop:
		return m.requestStop()
	case KeyExec:
		return m.requestExec()
	case KeyUp:
		if m.Level == LevelServices {
			return m.moveService(-1)
		}
		return m.move(-1)
	case KeyDown:
		if m.Level == LevelServices {
			return m.moveService(+1)
		}
		return m.move(+1)
	case KeyEnter:
		if m.Level == LevelSpaces {
			return m.EnterSpace()
		}
		return false
	case KeyBack:
		// Esc at spaces level is a no-op; quit is handled in the TUI layer.
		return m.BackLevel()
	case KeyFilter:
		m.Filter = (m.Filter + 1) % 4
		return true
	case KeySort:
		m.Sort = (m.Sort + 1) % 2
		m.applySort()
		return true
	case KeyVMLogs:
		m.ToggleVMLogs()
		return true
	case KeyLogs:
		m.ToggleLogs()
		return true
	default:
		return false
	}
}

// ToggleVMLogs opens the guest agent's own log regardless of what is
// selected. The machine the cockpit runs on is never in the selection list,
// so `l` — which follows the selection — can never reach it.
func (m *Model) ToggleVMLogs() {
	if m.LogsOpen && m.LogsVM {
		m.CloseLogs()
		return
	}
	if VMLogStream == nil {
		return
	}
	m.LogsOpen = true
	m.LogsVM = true
	m.LogsSpace = "vm"
	m.LogsLines = nil
	m.LogsScroll = -1
	m.LogsNote = ""
	m.LogsErr = ""
	m.LogsService = ""
	m.LogsCI = false
	m.LogsRepo = ""
	m.LogsTask = ""
	m.LogsPath = ""
}

// ToggleLogs opens the logs panel for the current selection (or closes it if
// already open). Non-supervised spaces have no control API: set an honest
// LogsErr and open the panel to show it, rather than streaming or hanging.
func (m *Model) ToggleLogs() {
	if m.LogsOpen {
		m.CloseLogs()
		return
	}
	// A host-run (space:false pipeline, e.g. publish) has no space/control API,
	// only a central run log keyed by task (its name). Tail that.
	if hr := m.SelectedHostRun(); hr != nil {
		m.LogsOpen = true
		m.LogsSpace = hr.Name
		m.LogsLines = nil
		m.LogsScroll = -1
		m.SearchMatch = -1
		m.LogsNote = ""
		m.LogsErr = ""
		m.LogsService = ""
		m.LogsRepo = "" // seam falls back to the cockpit cwd
		m.LogsTask = hr.Name
		if hr.HasLog && CILogStream != nil {
			m.LogsCI = true
		} else {
			m.LogsCI = false
			m.LogsErr = "no run log yet for " + hr.Name + " (run `sklp run " + hr.Name + "`)"
		}
		return
	}
	sp := m.Selection()
	if sp == nil {
		// Nothing selected — on a host with no spaces yet there is still one
		// thing worth reading: what the machine itself did on the way up
		// (mounts, dhcp, vsock). Without this, `l` on an empty cockpit does
		// nothing at all.
		if VMLogStream != nil {
			m.LogsOpen = true
			m.LogsVM = true
			m.LogsSpace = "vm"
			m.LogsLines = nil
			m.LogsScroll = -1
			m.LogsNote = ""
			m.LogsErr = ""
			m.LogsService = ""
			m.LogsCI = false
			m.LogsRepo = ""
			m.LogsTask = ""
			m.LogsPath = ""
		}
		return
	}
	m.LogsOpen = true
	m.LogsVM = false
	m.LogsSpace = sp.Name
	m.LogsNode = sp.Node
	m.LogsLines = nil
	m.LogsScroll = -1
	m.SearchMatch = -1
	m.LogsNote = ""
	m.LogsErr = ""
	m.LogsService = ""
	m.LogsCI = false
	m.LogsRepo = ""
	m.LogsTask = ""
	m.LogsPath = ""
	if sp.Node != "" {
		if !sp.Supervised {
			m.LogsErr = "no control API on " + sp.Node + " for this space"
			return
		}
		if len(sp.Services) == 0 {
			// The node's inventory reports containers, not services: stream the
			// space's own name and let the node resolve it.
			m.LogsService = sp.Name
			return
		}
		if svc := m.SelectedService(); svc != nil {
			m.LogsService = svc.Name
			return
		}
		m.LogsService = sp.Services[0].Name
		return
	}
	if sp.LimitedInfo || !sp.Supervised {
		// A non-supervised space has no control socket, but a CI run leaves its
		// log in the central store keyed by repo+task as ci-<task>-<ts>.log. The
		// task is the space name (always known); the repo comes from the bundle
		// when alive and falls back to the cockpit's cwd in the seam once the
		// space is torn down. The seam reports honestly if no such log exists.
		if CILogStream != nil {
			m.LogsCI = true
			m.LogsRepo = sp.Repo // "" → seam falls back to cwd
			m.LogsTask = sp.Name // task == space name
			return
		}
		m.LogsErr = "no control API: logs unavailable for this space"
		return
	}
	if len(sp.Services) == 0 {
		m.LogsErr = "space has no services to stream"
		return
	}
	// spaces-level l = first service (back-compat); services-level l = highlighted.
	if svc := m.SelectedService(); svc != nil {
		m.LogsService = svc.Name
		return
	}
	m.LogsService = sp.Services[0].Name
}

// CloseLogs closes the panel and clears its transient state.
func (m *Model) CloseLogs() {
	m.LogsOpen = false
	m.LogsService = ""
	m.LogsSpace = ""
	m.LogsNode = ""
	m.LogsLines = nil
	m.LogsScroll = -1
	m.LogsNote = ""
	m.LogsErr = ""
	m.LogsCI = false
	m.LogsRepo = ""
	m.LogsTask = ""
	m.LogsPath = ""
	m.SearchTyping = false
	m.SearchQuery = ""
	m.SearchMatch = -1
}

// AppendLogLine pushes a line into the ring buffer (dropping the oldest past
// the cap). When the view is pinned to the tail (follow mode) it stays pinned.
//
// The control API replays a service's whole history before it starts
// following, and there is no tail parameter to ask for less. The ring buffer
// therefore does the trimming: history beyond the cap is discarded as it
// arrives, so the panel settles on the LAST logBufferCap lines however long the
// service has been running.
func (m *Model) AppendLogLine(s string) {
	// The file-tailing seam announces its resolved path as the first line; it is
	// the only place the panel learns which run-log it actually got.
	if m.LogsPath == "" {
		if p, ok := strings.CutPrefix(s, "── tailing "); ok {
			m.LogsPath = strings.TrimSuffix(p, " ──")
		}
	}
	m.LogsLines = append(m.LogsLines, s)
	if len(m.LogsLines) > logBufferCap {
		m.LogsLines = m.LogsLines[len(m.LogsLines)-logBufferCap:]
	}
}

// CopyLogs puts the whole log buffer on the clipboard and reports the outcome in
// LogsNote. It copies the UNWRAPPED buffer lines: the pane wraps for display, and
// copying what the pane shows would hand back the fragments the operator is
// trying to escape.
func (m *Model) CopyLogs() bool {
	if len(m.LogsLines) == 0 {
		m.LogsNote = "nothing to copy yet"
		return true
	}
	if Clipboard == nil {
		m.LogsNote = "copy unavailable in this build"
		return true
	}
	if err := Clipboard(strings.Join(m.LogsLines, "\n") + "\n"); err != nil {
		m.LogsNote = "copy failed: " + err.Error()
		return true
	}
	m.LogsNote = itoa(len(m.LogsLines)) + " line(s) copied to clipboard"
	return true
}

// logRows is the buffer as the panel actually paints it: hard-wrapped to the
// pane width, so one entry is one screen row. Scrolling and rendering MUST agree
// on these units or a keypress moves by an amount the operator cannot predict.
func (m *Model) logRows() []string {
	return wrapLines(m.LogsLines, logsTextWidth(m))
}

// logsStep is one ↑/↓ press. A log line wider than the pane occupies several
// screen rows, so stepping by ONE row moved the view by a fraction of a line
// and took hundreds of presses to cross a full buffer — the arrows read as
// broken. A step is a small block of rows instead, scaled to the wrap factor so
// a press always advances at least one whole log line.
func (m *Model) logsStep() int {
	rows, lines := len(m.logRows()), len(m.LogsLines)
	step := logsStepLines
	if lines > 0 && rows > lines {
		// Average rows per buffer line, rounded up: one press ≈ logsStepLines
		// real log lines however hard they wrap.
		step = logsStepLines * ((rows + lines - 1) / lines)
	}
	if page := m.logsPage(); step > page {
		return page
	}
	return step
}

// logsStepLines is how many LOG LINES one ↑/↓ press moves through.
const logsStepLines = 3

// logsPage is one window worth of scrolling, minus a row of overlap so the reader
// keeps a line of context across the jump.
func (m *Model) logsPage() int {
	h := logsHeight(m.Height, logsPaneExtraRows(m))
	if h <= 1 {
		return 1
	}
	return h - 1
}

// ScrollLogsToTop pins the view to the oldest buffered row.
func (m *Model) ScrollLogsToTop() bool {
	if len(m.logRows()) == 0 {
		return false
	}
	if m.LogsScroll == 0 {
		return false
	}
	m.LogsScroll = 0
	return true
}

// FollowLogs re-pins the view to the tail so new lines keep arriving in view.
func (m *Model) FollowLogs() bool {
	if m.LogsScroll < 0 {
		return false
	}
	m.LogsScroll = -1
	return true
}

// ScrollLogs moves the top visible row by delta, clamped to the buffer. Moving
// up off the tail leaves follow mode; reaching the bottom re-enters it (-1).
//
// delta counts SCREEN rows, the same units the panel windows on: a log line
// wider than the pane occupies several rows, so counting buffer lines made one
// keypress jump by a variable amount and made maxTop far too small — the view
// snapped back to follow after a couple of presses.
func (m *Model) ScrollLogs(delta int) bool {
	n := len(m.logRows())
	if n == 0 {
		return false
	}
	height := logsHeight(m.Height, logsPaneExtraRows(m))
	// When following, the top visible line is the tail WINDOW's top (n-height),
	// not the last line — anchoring at n-1 would collapse the view to a couple
	// of lines on the first scroll. maxTop is the furthest-down top that still
	// fills the window; reaching it re-enters follow.
	maxTop := n - height
	if maxTop < 0 {
		maxTop = 0
	}
	top := m.LogsScroll
	if top < 0 {
		top = maxTop // tail-window anchor when following
	}
	top += delta
	if top < 0 {
		top = 0
	}
	if top >= maxTop {
		m.LogsScroll = -1 // back to follow (pinned to tail)
		return true
	}
	m.LogsScroll = top
	return true
}

// move shifts the selection with wrap-around. No-op on an empty list.
func (m *Model) move(delta int) bool {
	n := m.rowCount() // spaces + host-runs are one selectable list
	if n == 0 {
		return false
	}
	m.Selected = ((m.Selected+delta)%n + n) % n
	return true
}

// applySort reorders Spaces by the current Sort while keeping the selected
// space selected (selection follows the moved item, not the index).
func (m *Model) applySort() {
	sel := m.Selection()
	var selName string
	if sel != nil {
		selName = sel.Name
	}
	sortSpaces(m.Spaces, m.Sort)
	for i := range m.Spaces {
		if m.Spaces[i].Name == selName {
			m.Selected = i
			return
		}
	}
}

// sortSpaces orders spaces in place. Rows are ALWAYS grouped by node first:
// the view renders one table per node (see groupByNode), so a slice ordered
// across nodes would make ↑/↓ — which walks slice indices — jump between
// tables instead of down the screen. Local spaces sort first, then nodes
// alphabetically. Within a node, SortName is alphabetical and SortState floats
// the unhealthy ones first (errored, then not-all-ready), then alphabetical.
func sortSpaces(spaces []Space, mode Sort) {
	within := func(a, b Space) bool { return a.Name < b.Name }
	if mode == SortState {
		within = func(a, b Space) bool {
			if rank(a) != rank(b) {
				return rank(a) < rank(b)
			}
			return a.Name < b.Name
		}
	}
	less := func(a, b Space) bool {
		if a.Node != b.Node {
			// "" is the local node and must lead; otherwise compare by name.
			if a.Node == "" || b.Node == "" {
				return a.Node == ""
			}
			return a.Node < b.Node
		}
		return within(a, b)
	}
	// Simple insertion sort: the space list is tiny (a handful per host).
	for i := 1; i < len(spaces); i++ {
		for j := i; j > 0 && less(spaces[j], spaces[j-1]); j-- {
			spaces[j], spaces[j-1] = spaces[j-1], spaces[j]
		}
	}
}

// rank scores a space for SortState: lower is more urgent (shown first).
func rank(s Space) int {
	switch {
	case s.Err != "":
		return 0
	case s.LimitedInfo:
		return 2 // non-supervised: no readiness signal, treat as neutral
	case !s.AllReady:
		return 1
	default:
		return 2
	}
}
