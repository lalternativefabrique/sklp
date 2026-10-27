package cockpit

// The startup context picker: the cockpit is pointed at ONE node at a time, so
// the first screen asks which. Local is always offered first; the rest are the
// configured deploy contexts, listed from config alone — no node is dialled
// until one is chosen, so a powered-off VM costs nothing to have configured.

// LocalContext is the active-context value naming this machine, and the label
// the picker lists it under.
const LocalContext = ""

// localLabel is how the local machine reads in the picker and the header.
const localLabel = "local"

// ContextChoice is one row of the picker.
type ContextChoice struct {
	// Name is the config context name; "" (LocalContext) is this machine.
	Name string
	// Target is the server address, empty for local.
	Target string
}

// Label is what the picker prints for the choice.
func (c ContextChoice) Label() string {
	if c.Name == LocalContext {
		return localLabel
	}
	return c.Name
}

// contextChoices is local plus every configured context, in the seam's order.
func contextChoices() []ContextChoice {
	out := []ContextChoice{{Name: LocalContext}}
	if ContextList == nil {
		return out
	}
	for _, e := range ContextList() {
		out = append(out, ContextChoice{Name: e.Name, Target: e.Target})
	}
	return out
}

// OpenContexts opens the picker, highlighting the active context so confirming
// straight away is a no-op rather than a surprise switch.
func (m *Model) OpenContexts() bool {
	if m.LogsOpen || m.JumpOpen {
		return false
	}
	m.ContextsOpen = true
	m.ContextChoices = contextChoices()
	m.ContextIndex = 0
	for i, c := range m.ContextChoices {
		if c.Name == m.Context {
			m.ContextIndex = i
			break
		}
	}
	m.Note = ""
	return true
}

// CloseContexts dismisses the picker without switching. It refuses to close
// the very first one: the cockpit has no context to fall back to before a
// choice is made, so Esc there would leave an empty screen.
func (m *Model) CloseContexts() bool {
	if !m.ContextsOpen || !m.ContextChosen {
		return false
	}
	m.ContextsOpen = false
	return true
}

// moveContext walks the picker list with wrap-around.
func (m *Model) moveContext(delta int) bool {
	n := len(m.ContextChoices)
	if !m.ContextsOpen || n == 0 {
		return false
	}
	m.ContextIndex = ((m.ContextIndex+delta)%n + n) % n
	return true
}

// ConfirmContext activates the highlighted context and closes the picker. It
// reports whether the active context actually changed, so the caller knows to
// drop the previous node's rows and re-read rather than keep showing them.
func (m *Model) ConfirmContext() bool {
	if !m.ContextsOpen || m.ContextIndex < 0 || m.ContextIndex >= len(m.ContextChoices) {
		return false
	}
	pick := m.ContextChoices[m.ContextIndex]
	switched := !m.ContextChosen || pick.Name != m.Context
	m.Context = pick.Name
	m.ContextsOpen = false
	m.ContextChosen = true
	if switched {
		// The previous node's inventory describes a machine we are no longer
		// pointed at; showing it until the next read would be a lie.
		m.Spaces = nil
		m.HostRuns = nil
		m.Selected = 0
		m.Level = LevelSpaces
		m.SvcIndex = 0
	}
	return switched
}

// ContextLabel names the active context for the header.
func (m *Model) ContextLabel() string {
	if m.Context == LocalContext {
		return localLabel
	}
	return m.Context
}
