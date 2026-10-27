package cockpit

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

// Onto is the ref the flow lifecycle builds on / diffs against.
const Onto = "main"

// FlowRead is the read-only flow source the cockpit needs. It is injected by the
// command layer (cockpit.go) because the real implementation lives in
// cmd/sklp/internal/flow, which the internal-package boundary forbids this
// package from importing directly. The seam keeps state.go free of that import
// while still reading the REAL source — nil yields a collapsed banner.
var FlowRead func() FlowState

// SpaceSummary is the full-inventory row, translated from spaceapi.SpaceInfo by
// the command layer so cockpit stays decoupled from the embedded runtime.
type SpaceSummary struct {
	Name   string
	Status string
	// Runc are the space's containers as the runc state reports them — the
	// CONTAINER-level truth, available with or without a control API. Carried in
	// full (not folded to a count) so a non-supervised space can still be drilled
	// into and exec'd: without it the cockpit knows a space runs 6 containers but
	// not which, and can offer nothing to enter.
	Runc       []ContainerSummary
	Supervised bool
	// Volumes names the space's named volumes. Teardown destroys them with the
	// space, so the confirm prompt must be able to say what data is at stake.
	Volumes []string
	// Repo is the host repo path bind-mounted at /workspace, recovered from the
	// space bundle. Lets the cockpit tail the right repo's central CI log.
	Repo string
	// Services is the supervised view — real service names and readiness, read
	// from the control API. Nil when only the container level is available, which
	// is what LimitedInfo then reports.
	Services []ServiceSummary
	AllReady bool
}

// ServiceSummary is one supervised service as the control API reports it. State
// is the control layer's free-form string, mapped onto the State enum here so
// the command layer stays out of that vocabulary.
type ServiceSummary struct {
	Name      string
	State     string
	Instances int
}

// ContainerSummary is one runc container of a space: its full runc id and the
// runc status string, straight from the state.
type ContainerSummary struct {
	ID     string
	Status string
}

// SpaceList is the full-inventory seam (mirrors `space ls`). Injected by the
// command layer; nil falls back to the supervised-only control.ListSpaces path.
var SpaceList func() []SpaceSummary

// RemoteNode is one configured remote node (a VM or pod running spaces) and
// what it answered. Spaces is its inventory; Err is why it could not be read,
// and the two are mutually exclusive — a node that failed is LISTED with its
// error rather than dropped, so an unreachable machine is visibly unreachable
// instead of silently absent.
type RemoteNode struct {
	// Name is the config context name, the identity the operator already knows.
	Name string
	// Target is the server address, shown so a wrong context is recognisable.
	Target string
	Spaces []SpaceSummary
	Err    error
}

// RemoteNodeList reads every configured remote node's inventory, concurrently.
// Injected by the command layer (it owns config + the control client); nil
// yields a local-only cockpit, exactly as before.
//
// Implementations MUST bound each node's read and never let one slow or
// unreachable machine hold the refresh: the cockpit redraws on a timer, and a
// hung node would freeze the whole view.
var RemoteNodeList func(ctx context.Context) []RemoteNode

// ContextList names every node the cockpit can be pointed at, WITHOUT touching
// the network: the picker must draw instantly at startup, so it reads the
// configured contexts only. The local machine is not included; the picker adds
// it itself as the always-present first entry. Injected by the command layer;
// nil yields a local-only picker.
var ContextList func() []ContextEntry

// ContextEntry is one selectable node in the startup picker: the config context
// name and its server address, shown so a wrong context is recognisable before
// anything is dialled.
type ContextEntry struct {
	Name   string
	Target string
}

// SpaceExec is the interactive-shell seam: it opens a shell in one container of
// a space, taking over the terminal for the duration. The TUI layer must have
// released the terminal first (see App.Suspend) — this runs a foreground process
// with its own PTY. node names the machine the space runs on ("" = local, the
// embedded runtime over the Unix socket; otherwise the configured node, reached
// over its control API), and container is a runc id only when local — a remote
// target is named by service. Injected by the command layer; nil makes the
// action report that it is unavailable.
var SpaceExec func(node, space, container string) error

// HostRun is a `kind: pipeline, space: false` pipeline — one that runs directly
// on the host (e.g. publish), so it never creates a space and thus never shows
// in the Spaces list. The cockpit lists these separately, with their central
// run log tailable, so host-only runs aren't invisible.
type HostRun struct {
	// Name is the pipeline name (== its central log task key).
	Name string
	// HasLog is true when a central run log exists for this pipeline (a past run).
	HasLog bool
}

// VMStatRead reports what the guest can tell about the machine it runs on.
// Left nil the VM panel is simply absent, exactly as HostRunList behaves.
var VMStatRead func() VMState

// VMLogStream tails the guest agent's own log. Left nil, `l` on an empty
// cockpit stays inert as before.
var VMLogStream func(ctx context.Context, out func(string)) error

// HostRunList is the host-only pipelines seam: it reads .sklp/pipelines/*.yaml,
// keeps those with space:false, and reports each with whether a run log exists.
// Injected by the command layer; nil yields no host-runs section.
var HostRunList func() []HostRun

// KillSpace is the destructive teardown seam (mirrors `space down`, volumes
// kept). Injected by the command layer (runtime.NewRegistry().Down); nil makes
// the kill action a no-op so the pure model stays testable.
var KillSpace func(name string) error

// StopSpace is the non-destructive halt seam (mirrors `space stop`): containers
// and network down, named volumes kept. Injected by the command layer
// (runtime.NewRegistry().Stop); nil makes the action a no-op.
var StopSpace func(name string) error

// PurgeSpaces is the bulk teardown seam (mirrors `sklp clean --deep`, volumes
// kept): it tears down every space with no running container and returns their
// names. Spaces holding a live container are kept by the registry itself, so a
// running deployment survives a purge. nil makes the action a no-op.
var PurgeSpaces func() ([]string, error)

// LogStream is the live-logs seam. It follows svc's logs in space, invoking
// onLine per received line until ctx is cancelled. node names the machine the
// space runs on ("" = local, reached over the Unix socket; otherwise the
// configured node, reached over its control API). Injected by the command
// layer; nil disables streaming.
var LogStream func(ctx context.Context, node, space, svc string, onLine func(string)) error

// CILogStream is the CI-run logs seam: a non-supervised space has no control
// socket (the runner exits when the run converges), but a CI run leaves its
// verbose output in the central store keyed by repo+task
// (<cache>/sklp/logs/<repo-slug>/ci-<task>-<ts>.log). This seam tails the most
// recent such file for the given repo and task (the space name), invoking onLine
// per line until ctx is cancelled. It returns the resolved file path (for the
// panel header) or an error when no run log exists. Injected by the command
// layer; nil disables it.
var CILogStream func(ctx context.Context, repo, task string, onLine func(string)) (string, error)

// Clipboard is the copy-out seam: the cockpit runs in an alt-screen TUI where a
// mouse selection grabs whatever the pane happened to wrap, so copying the log
// buffer needs an explicit action. Injected by the command layer (wl-copy /
// xclip / pbcopy); nil makes [c] report that no clipboard tool was found.
var Clipboard func(s string) error

// Load builds a fresh Model from the REAL state sources. It is read-only and
// degrades gracefully: a failing source populates an honest error/Unknown
// rather than fabricating data. Existing selection/filter/sort are preserved by
// the caller via Refresh; Load itself returns a defaulted Model.
func Load(ctx context.Context, node string) *Model {
	m := &Model{Context: node}
	return m.ApplySnapshot(ReadSnapshot(ctx, node))
}

// loadVM reads the guest's own machine state via the VMStatRead seam. A nil
// seam (unit tests, or a host with nothing to report) yields no VM panel.
func loadVM() VMState {
	if VMStatRead == nil {
		return VMState{}
	}
	return VMStatRead()
}

// loadHostRuns reads the host-only pipelines via the HostRunList seam. A nil
// seam (unit tests) yields no host-runs section.
func loadHostRuns() []HostRun {
	if HostRunList == nil {
		return nil
	}
	return HostRunList()
}

// Snapshot is one read of the real state sources, taken OFF the UI event loop.
// Reading a remote node is a network round-trip: doing it inline in the event
// loop freezes input and log rendering for as long as the slowest node takes to
// answer (up to its timeout), so the read is separated from the apply.
type Snapshot struct {
	Flow     FlowState
	Spaces   []Space
	HostRuns []HostRun
	VM       VMState
}

// ReadSnapshot performs every blocking source read. It touches no Model, so it
// is safe to call from a goroutine while the UI keeps drawing.
func ReadSnapshot(ctx context.Context, node string) Snapshot {
	return Snapshot{
		Flow:     loadFlow(),
		Spaces:   loadSpaces(ctx, node),
		HostRuns: loadHostRuns(),
		VM:       loadVM(),
	}
}

// remoteNodesFor reads only the ACTIVE context, so pointing the cockpit at one
// node never dials the others: an unrelated powered-off VM must cost nothing.
func remoteNodesFor(ctx context.Context, node string) []RemoteNode {
	var out []RemoteNode
	for _, n := range RemoteNodeList(ctx) {
		if n.Name == node {
			out = append(out, n)
		}
	}
	return out
}

// Refresh re-reads the sources into prev, preserving the user's selection,
// filter, and sort. It returns prev for chaining. It blocks on I/O; the TUI
// uses ReadSnapshot + ApplySnapshot instead so the event loop never stalls.
func (m *Model) Refresh(ctx context.Context) *Model {
	return m.ApplySnapshot(ReadSnapshot(ctx, m.Context))
}

// ApplySnapshot folds an already-read Snapshot into the model, preserving the
// user's selection, filter and sort. It performs no I/O.
func (m *Model) ApplySnapshot(snap Snapshot) *Model {
	selName := ""
	if s := m.Selection(); s != nil {
		selName = s.Name
	}
	m.Flow = snap.Flow
	m.Spaces = snap.Spaces
	sortSpaces(m.Spaces, m.Sort)
	m.Host = rollup(m.Spaces)
	m.HostRuns = snap.HostRuns
	m.VM = snap.VM
	m.Err = ""
	// Re-anchor the selection on the same space name; clamp if it vanished.
	m.Selected = 0
	for i := range m.Spaces {
		if m.Spaces[i].Name == selName {
			m.Selected = i
			break
		}
	}
	// Keep the drill-in level honest: fall back to spaces when the entered space
	// is gone or emptied, and clamp the service highlight to the new list.
	if m.Level == LevelServices {
		sp := m.Selection()
		switch {
		case sp == nil || len(sp.Services) == 0:
			m.Level = LevelSpaces
			m.SvcIndex = 0
		case m.SvcIndex > len(sp.Services)-1:
			m.SvcIndex = len(sp.Services) - 1
		}
	}
	return m
}

// loadFlow reads the flow banner via the injected FlowRead seam. A nil seam
// (e.g. in unit tests) yields a collapsed banner rather than fabricated data.
func loadFlow() FlowState {
	if FlowRead == nil {
		return FlowState{Onto: Onto, CI: MilestoneUnknown, PR: MilestoneUnknown}
	}
	return FlowRead()
}

// loadSpaces enumerates the FULL inventory via the SpaceList seam (same source
// as `sklp space ls`). Supervised spaces are enriched with services via the
// control API; non-supervised ones are listed honestly with limited info.
// A nil seam falls back to the supervised-only control.ListSpaces path.
func loadSpaces(ctx context.Context, node string) []Space {
	if node != LocalContext {
		// A remote context shows that node's spaces only: the local socket
		// describes THIS machine, which is not what the operator selected.
		return loadRemoteSpaces(ctx, node)
	}
	var out []Space
	if SpaceList == nil {
		out = loadSupervisedOnly(ctx)
	} else {
		infos := SpaceList()
		out = make([]Space, 0, len(infos))
		for _, info := range infos {
			if info.Supervised {
				out = append(out, loadSpace(ctx, info.Name))
				continue
			}
			out = append(out, limitedSpace("", info))
		}
	}
	return out
}

// loadRemoteSpaces appends the spaces of every configured remote node. They are
// rendered as limited-info rows: their supervisor is reachable over the
// network, but the cockpit's logs/exec seams speak to the LOCAL socket, so
// claiming a full drill-in here would offer actions that cannot run.
//
// A node that failed to answer yields a single row carrying the error, so the
// machine stays visible with its reason instead of vanishing from the list.
func loadRemoteSpaces(ctx context.Context, node string) []Space {
	if RemoteNodeList == nil || node == LocalContext {
		return nil
	}
	var out []Space
	for _, node := range remoteNodesFor(ctx, node) {
		if node.Err != nil {
			out = append(out, Space{
				Name:        node.Target,
				Node:        node.Name,
				Err:         node.Err.Error(),
				LimitedInfo: true,
			})
			continue
		}
		for _, info := range node.Spaces {
			out = append(out, limitedSpace(node.Name, info))
		}
	}
	return out
}

// limitedSpace renders a remote space. The inventory is container-level (name,
// status, containers, volumes), which is all a space without a control API has.
// When the node also answered with its supervised state, those real services —
// with readiness and the names the control API keys on — replace the derived
// container rows and the space is no longer limited.
func limitedSpace(node string, info SpaceSummary) Space {
	sp := Space{
		Name:        info.Name,
		Node:        node,
		Status:      info.Status,
		Containers:  len(info.Runc),
		Services:    containerServices(info),
		Volumes:     info.Volumes,
		Repo:        info.Repo,
		Supervised:  info.Supervised,
		LimitedInfo: true,
	}
	if len(info.Services) > 0 {
		sp.Services = supervisedServices(info)
		sp.AllReady = info.AllReady
		sp.LimitedInfo = false
	}
	return sp
}

// supervisedServices renders the control API's service list, pairing each
// service with one of its runc containers from the same inventory. The container
// is what `[e]` enters: the supervised view names services, not instances, so
// without this pairing an exec would have to resolve the id itself.
func supervisedServices(info SpaceSummary) []Service {
	byName := make(map[string]string, len(info.Runc))
	for _, c := range info.Runc {
		name := serviceNameFromID(c.ID, info.Name)
		if _, seen := byName[name]; !seen {
			byName[name] = c.ID
		}
	}
	out := make([]Service, 0, len(info.Services))
	for _, svc := range info.Services {
		out = append(out, Service{
			Name:      svc.Name,
			State:     mapState(svc.State),
			Container: byName[svc.Name],
			Instances: svc.Instances,
		})
	}
	return out
}

// containerServices derives the drill-in rows of a NON-supervised space from its
// runc containers. This is the container-level truth (name + status), which is
// all a space without a control API has: no readiness, no captured logs. It is
// still enough to see what runs and to exec into it, so the rows exist rather
// than the space being a dead end.
func containerServices(info SpaceSummary) []Service {
	if len(info.Runc) == 0 {
		return nil
	}
	out := make([]Service, 0, len(info.Runc))
	at := make(map[string]int, len(info.Runc))
	for _, c := range info.Runc {
		name := serviceNameFromID(c.ID, info.Name)
		if i, seen := at[name]; seen {
			out[i].Instances++
			if out[i].State != StateReady {
				out[i].State = mapState(c.Status)
			}
			continue
		}
		at[name] = len(out)
		out = append(out, Service{
			Name:      name,
			State:     mapState(c.Status),
			Container: c.ID,
			Instances: 1,
		})
	}
	return out
}

// serviceNameFromID recovers the service name from a runc container id, which is
// built as `sklp-deploy-<space>-<svc>-<instance>` (space_scope.containerID).
// Trimming that prefix is what makes the list readable — the raw ids repeat the
// space's content hash on every row and push the distinguishing part off-screen.
// The instance suffix goes too: the control API keys logs and restarts by
// service name, so a row named `core-0` is a 404 there. An id that does not
// carry the prefix is shown whole rather than guessed at.
func serviceNameFromID(id, space string) string {
	for _, prefix := range []string{"sklp-deploy-" + space + "-", "sklp-deploy-"} {
		if trimmed := strings.TrimPrefix(id, prefix); trimmed != id && trimmed != "" {
			return trimInstanceSuffix(trimmed)
		}
	}
	return id
}

// instanceSuffix matches the trailing `-<n>` a launcher appends per replica.
var instanceSuffix = regexp.MustCompile(`-\d+$`)

// trimInstanceSuffix drops that suffix unless it is the whole name.
func trimInstanceSuffix(name string) string {
	if trimmed := instanceSuffix.ReplaceAllString(name, ""); trimmed != "" {
		return trimmed
	}
	return name
}

// loadSupervisedOnly is the defensive fallback when SpaceList is unset.
func loadSupervisedOnly(ctx context.Context) []Space {
	names, err := control.ListSpaces()
	if err != nil || len(names) == 0 {
		return nil
	}
	out := make([]Space, 0, len(names))
	for _, name := range names {
		out = append(out, loadSpace(ctx, name))
	}
	return out
}

func loadSpace(ctx context.Context, name string) Space {
	sp := Space{Name: name, Supervised: true}
	client, err := control.Dial(name)
	if err != nil {
		sp.Err = err.Error()
		return sp
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	st, err := client.State(cctx)
	if err != nil {
		sp.Err = err.Error()
		return sp
	}
	sp.AllReady = st.AllReady
	sp.Services = make([]Service, 0, len(st.Services))
	for _, svc := range st.Services {
		sp.Services = append(sp.Services, Service{
			Name:      svc.Name,
			State:     mapState(svc.State),
			Instances: len(svc.Instances),
		})
	}
	return sp
}

// mapState maps the control layer's free-form state string onto our State enum.
// The strings are not a closed set in protocol.go, so unknown values degrade to
// StateNone rather than guessing.
func mapState(s string) State {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "ready", "running", "up", "healthy":
		return StateReady
	case "starting", "pending", "reconciling", "restarting":
		return StateBusy
	case "idle", "stopped", "exited", "done":
		return StateIdle
	case "error", "failed", "crashed", "unhealthy", "degraded":
		return StateError
	default:
		return StateNone
	}
}

// rollup aggregates the host-wide counts from the loaded spaces.
func rollup(spaces []Space) HostRollup {
	r := HostRollup{Spaces: len(spaces)}
	for _, sp := range spaces {
		if sp.Supervised {
			r.Supervised++
		}
		if sp.Err != "" {
			r.Errored++
		}
		for _, svc := range sp.Services {
			r.Services++
			switch svc.State {
			case StateReady:
				r.Ready++
			case StateBusy:
				r.Busy++
			case StateError:
				r.Errored++
			}
		}
	}
	return r
}
