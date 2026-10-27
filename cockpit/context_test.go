package cockpit

import (
	"context"
	"strings"
	"testing"
)

func withContexts(t *testing.T, entries []ContextEntry) {
	t.Helper()
	ContextList = func() []ContextEntry { return entries }
	t.Cleanup(func() { ContextList = nil })
}

var twoContexts = []ContextEntry{
	{Name: "ci-instance-v1", Target: "https://10.0.0.4:6464"},
	{Name: "node-b", Target: "https://10.0.0.9:6464"},
}

// The picker always offers local first, then every configured context.
func TestContextChoicesLeadWithLocal(t *testing.T) {
	withContexts(t, twoContexts)
	m := &Model{}
	m.OpenContexts()

	if len(m.ContextChoices) != 3 {
		t.Fatalf("want local + 2 contexts, got %d", len(m.ContextChoices))
	}
	if m.ContextChoices[0].Name != LocalContext || m.ContextChoices[0].Label() != "local" {
		t.Fatalf("first choice must be local: %+v", m.ContextChoices[0])
	}
	if m.ContextChoices[1].Name != "ci-instance-v1" {
		t.Fatalf("configured contexts must follow: %+v", m.ContextChoices[1])
	}
}

// With no configured context the picker still offers local, so the cockpit is
// usable on a workstation with no deploy contexts at all.
func TestContextChoicesLocalOnly(t *testing.T) {
	m := &Model{}
	m.OpenContexts()
	if len(m.ContextChoices) != 1 || m.ContextChoices[0].Name != LocalContext {
		t.Fatalf("want local only, got %+v", m.ContextChoices)
	}
}

// Confirming a context activates it and reports the switch.
func TestConfirmContextActivates(t *testing.T) {
	withContexts(t, twoContexts)
	m := &Model{}
	m.OpenContexts()
	m.Update(KeyDown)
	m.Update(KeyDown)

	if !m.Update(KeyEnter) {
		t.Fatal("confirming a new context must report a switch")
	}
	if m.Context != "node-b" {
		t.Fatalf("Context=%q want node-b", m.Context)
	}
	if m.ContextsOpen {
		t.Fatal("picker must close on confirm")
	}
	if !m.ContextChosen {
		t.Fatal("ContextChosen must be set after the first pick")
	}
}

// Switching context must drop the previous node's rows: showing another
// machine's inventory under a new context name would be a lie.
func TestConfirmContextDropsPreviousInventory(t *testing.T) {
	withContexts(t, twoContexts)
	m := &Model{
		Spaces:   []Space{{Name: "local-one"}, {Name: "local-two"}},
		Selected: 1,
		Level:    LevelServices,
		SvcIndex: 3,
	}
	m.Context = LocalContext
	m.ContextChosen = true
	m.OpenContexts()
	m.Update(KeyDown)
	m.Update(KeyEnter)

	if len(m.Spaces) != 0 {
		t.Fatalf("stale rows kept after a switch: %+v", m.Spaces)
	}
	if m.Selected != 0 || m.Level != LevelSpaces || m.SvcIndex != 0 {
		t.Fatalf("drill-in not reset: sel=%d level=%v svc=%d", m.Selected, m.Level, m.SvcIndex)
	}
}

// Re-confirming the SAME context is a no-op, not a reset: reopening the picker
// and pressing enter must not blank the view.
func TestConfirmSameContextKeepsInventory(t *testing.T) {
	withContexts(t, twoContexts)
	m := &Model{Spaces: []Space{{Name: "keep-me"}}}
	m.Context = LocalContext
	m.ContextChosen = true
	m.OpenContexts()

	if m.Update(KeyEnter) {
		t.Fatal("re-picking the active context must not report a switch")
	}
	if len(m.Spaces) != 1 {
		t.Fatalf("inventory must survive a no-op pick: %+v", m.Spaces)
	}
}

// The picker highlights the active context when reopened.
func TestOpenContextsHighlightsActive(t *testing.T) {
	withContexts(t, twoContexts)
	m := &Model{Context: "node-b", ContextChosen: true}
	m.OpenContexts()
	if got := m.ContextChoices[m.ContextIndex].Name; got != "node-b" {
		t.Fatalf("highlighted %q want the active context", got)
	}
}

// The FIRST picker is modal: Esc cannot dismiss it, because there is no
// inventory behind it to fall back to.
func TestFirstPickerCannotBeDismissed(t *testing.T) {
	withContexts(t, twoContexts)
	m := &Model{}
	m.OpenContexts()
	m.Update(KeyBack)
	if !m.ContextsOpen {
		t.Fatal("the first picker must stay open until a context is chosen")
	}
}

// Once a context is active, Esc closes the reopened picker without switching.
func TestReopenedPickerIsDismissable(t *testing.T) {
	withContexts(t, twoContexts)
	m := &Model{Context: LocalContext, ContextChosen: true}
	m.OpenContexts()
	m.Update(KeyDown)
	m.Update(KeyBack)

	if m.ContextsOpen {
		t.Fatal("esc must close a reopened picker")
	}
	if m.Context != LocalContext {
		t.Fatalf("esc must not switch context, got %q", m.Context)
	}
}

// [C] reopens the picker from the cockpit view.
func TestKeyContextReopensPicker(t *testing.T) {
	withContexts(t, twoContexts)
	m := &Model{Context: LocalContext, ContextChosen: true}
	if !m.Update(KeyContext) {
		t.Fatal("C must open the picker")
	}
	if !m.ContextsOpen {
		t.Fatal("picker must be open after C")
	}
}

// Selecting a remote context reads THAT node only — the other configured nodes
// are never dialled, which is what keeps a powered-off VM free.
func TestRemoteContextReadsOnlyItsOwnNode(t *testing.T) {
	RemoteNodeList = func(ctx context.Context) []RemoteNode {
		return []RemoteNode{
			{Name: "ci-instance-v1", Spaces: []SpaceSummary{{Name: "ci-space"}}},
			{Name: "node-b", Spaces: []SpaceSummary{{Name: "prod-space"}}},
		}
	}
	t.Cleanup(func() { RemoteNodeList = nil })

	got := loadSpaces(context.Background(), "node-b")
	if len(got) != 1 || got[0].Name != "prod-space" {
		t.Fatalf("want only the selected node's spaces, got %+v", got)
	}
	if got[0].Node != "node-b" {
		t.Fatalf("row not tagged with its node: %+v", got[0])
	}
}

// The local context must not pull in remote rows at all.
func TestLocalContextReadsNoRemotes(t *testing.T) {
	RemoteNodeList = func(ctx context.Context) []RemoteNode {
		t.Fatal("local context must not dial any remote node")
		return nil
	}
	SpaceList = func() []SpaceSummary { return nil }
	t.Cleanup(func() { RemoteNodeList = nil; SpaceList = nil })

	if got := loadSpaces(context.Background(), LocalContext); len(got) != 0 {
		t.Fatalf("want no spaces, got %+v", got)
	}
}

// The header names the active context: mistaking prod for local is the costly
// confusion, so the label must always be on screen.
func TestHeaderShowsActiveContext(t *testing.T) {
	m := &Model{Context: "node-b", ContextChosen: true, Width: 120, Height: 40}
	out := Render(m, NewPlainRenderer())
	if !strings.Contains(out, "node-b") {
		t.Fatalf("header must name the active context:\n%s", out)
	}
}

// The picker view lists every choice with its target.
func TestPickerRendersChoices(t *testing.T) {
	withContexts(t, twoContexts)
	m := &Model{Width: 120, Height: 40}
	m.OpenContexts()

	out := Render(m, NewPlainRenderer())
	for _, want := range []string{"local", "ci-instance-v1", "node-b", "https://10.0.0.9:6464"} {
		if !strings.Contains(out, want) {
			t.Fatalf("picker must show %q:\n%s", want, out)
		}
	}
}
