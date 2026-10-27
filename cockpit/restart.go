package cockpit

import "strings"

// Relaunching a stopped dev space. `sklp dev` is a FOREGROUND supervisor, not a
// daemon: there is no process to signal back to life, so the only honest restart
// is to run it again in the operator's terminal — the same hand-over [e] makes
// for a shell. The model can therefore only arm the request; the TUI layer
// performs it.

// devSpacePrefix is what `sklp dev` names its space: sklp-dev-<scope-hash>.
// Restart is offered on those rows only — a CI or deploy space has no dev
// supervisor to re-run.
const devSpacePrefix = "sklp-dev-"

// RestartRequest names the dev run to relaunch.
type RestartRequest struct {
	// Space is the stopped space the row belongs to, used to resolve the stack.
	Space string
	// Repo is the host repo the dev stack lives in (from the space bundle).
	Repo string
}

// RestartDev is the relaunch seam: it re-runs `sklp dev` for the stack the given
// space belongs to, taking over the terminal for as long as the supervisor runs.
// Injected by the command layer; nil makes the action report itself unavailable.
var RestartDev func(space, repo string) error

// IsDevSpace reports whether a space was created by `sklp dev`.
func IsDevSpace(name string) bool { return strings.HasPrefix(name, devSpacePrefix) }

// requestRestart arms the relaunch of the selected stopped dev space. It refuses
// anything it cannot honestly restart: a running space (nothing to do), a
// supervised deploy (a different lifecycle), a non-dev space (no supervisor),
// and a space whose repo is unknown (the bundle is gone, so there is no
// directory to run `sklp dev` in).
func (m *Model) requestRestart() bool {
	if m.Level != LevelSpaces || m.LogsOpen {
		return false
	}
	sp := m.Selection()
	if sp == nil {
		return false
	}
	if sp.Supervised {
		m.Note = sp.Name + " is a supervised deploy — use `sklp deploy` to bring it back"
		return true
	}
	if !IsDevSpace(sp.Name) {
		m.Note = sp.Name + " is not a dev space — [r] only relaunches `sklp dev` runs"
		return true
	}
	if sp.Containers > 0 && sp.Status != statusStopped {
		m.Note = sp.Name + " is still running — [s] stop it first"
		return true
	}
	if sp.Repo == "" {
		m.Note = "cannot tell which repo " + sp.Name + " belongs to — run `sklp dev` from it directly"
		return true
	}
	if RestartDev == nil {
		m.Note = "restart unavailable in this build"
		return true
	}
	m.PendingRestart = &RestartRequest{Space: sp.Name, Repo: sp.Repo}
	m.Note = ""
	return true
}

// TakePendingRestart hands the armed request to the TUI layer and disarms it, so
// one keypress launches exactly one supervisor.
func (m *Model) TakePendingRestart() *RestartRequest {
	req := m.PendingRestart
	m.PendingRestart = nil
	return req
}
