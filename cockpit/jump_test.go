package cockpit

import "testing"

func TestOpenJumpClosedByLogsPanel(t *testing.T) {
	m := &Model{LogsOpen: true}
	if m.OpenJump() {
		t.Fatal("the palette must not open while the logs panel owns the screen")
	}
	if m.JumpOpen {
		t.Fatal("JumpOpen must stay false when OpenJump refused")
	}
}

func TestJumpEmptyQueryMatchesNothing(t *testing.T) {
	m := &Model{Spaces: []Space{{Name: "alpha"}}}
	m.OpenJump()
	if hits := m.jumpHits(); len(hits) != 0 {
		t.Fatalf("an empty query must not list every space, got %d hits", len(hits))
	}
}

func TestJumpMatchesSpaceAndService(t *testing.T) {
	m := &Model{Spaces: []Space{
		{Name: "shop", Services: []Service{{Name: "api"}, {Name: "worker"}}},
		{Name: "acme", Services: []Service{{Name: "api"}}},
	}}
	m.OpenJump()
	for _, r := range "api" {
		m.JumpInput(r)
	}
	hits := m.jumpHits()
	if len(hits) != 2 {
		t.Fatalf("both spaces' 'api' service should match, got %d: %+v", len(hits), hits)
	}
	if hits[0].label != "shop › api" || hits[1].label != "acme › api" {
		t.Fatalf("unexpected labels: %q, %q", hits[0].label, hits[1].label)
	}
}

func TestJumpConfirmSelectsSpace(t *testing.T) {
	m := &Model{Spaces: []Space{
		{Name: "alpha"},
		{Name: "shop", Services: []Service{{Name: "api"}}},
	}}
	m.OpenJump()
	for _, r := range "shop" {
		m.JumpInput(r)
	}
	if !m.ConfirmJump() {
		t.Fatal("ConfirmJump should report a change")
	}
	if m.JumpOpen {
		t.Fatal("confirming must close the palette")
	}
	if m.Selected != 1 {
		t.Fatalf("selection should land on shop (index 1), got %d", m.Selected)
	}
	if m.Level != LevelSpaces {
		t.Fatalf("a space-only hit must not drill in, got level %v", m.Level)
	}
}

func TestJumpConfirmOnContainerDrillsIn(t *testing.T) {
	m := &Model{Spaces: []Space{
		{Name: "alpha", Services: []Service{{Name: "web"}, {Name: "worker"}}},
	}}
	m.OpenJump()
	for _, r := range "worker" {
		m.JumpInput(r)
	}
	m.ConfirmJump()
	if m.Selected != 0 {
		t.Fatalf("selection should land on alpha (index 0), got %d", m.Selected)
	}
	if m.Level != LevelServices {
		t.Fatalf("a container hit must drill straight into the service list, got level %v", m.Level)
	}
	if m.SvcIndex != 1 {
		t.Fatalf("SvcIndex should point at 'worker' (index 1), got %d", m.SvcIndex)
	}
}

func TestJumpMoveClampsToResults(t *testing.T) {
	m := &Model{Spaces: []Space{{Name: "alpha"}, {Name: "alphabet"}}}
	m.OpenJump()
	for _, r := range "alpha" {
		m.JumpInput(r)
	}
	if len(m.jumpHits()) != 2 {
		t.Fatalf("query should match both spaces, got %d", len(m.jumpHits()))
	}
	if !m.moveJump(+1) || m.JumpIndex != 1 {
		t.Fatalf("moveJump(+1) should land on index 1, got %d", m.JumpIndex)
	}
	if m.moveJump(+1) {
		t.Fatal("moveJump past the last result must be a no-op")
	}
	if m.JumpIndex != 1 {
		t.Fatalf("index must stay clamped at 1, got %d", m.JumpIndex)
	}
}

func TestJumpBackspaceClearsIndex(t *testing.T) {
	m := &Model{Spaces: []Space{{Name: "alpha"}, {Name: "alphabet"}}}
	m.OpenJump()
	for _, r := range "alpha" {
		m.JumpInput(r)
	}
	m.moveJump(+1)
	m.JumpBackspace()
	if m.JumpIndex != 0 {
		t.Fatalf("editing the query should re-clamp the highlight to 0, got %d", m.JumpIndex)
	}
}

func TestUpdateKeyJumpOpensAndCloses(t *testing.T) {
	m := &Model{Spaces: []Space{{Name: "alpha"}}}
	if !m.Update(KeyJump) {
		t.Fatal("KeyJump should open the palette")
	}
	if !m.JumpOpen {
		t.Fatal("palette must be open after KeyJump")
	}
	if !m.Update(KeyJump) {
		t.Fatal("KeyJump again should close the palette")
	}
	if m.JumpOpen {
		t.Fatal("palette must be closed after a second KeyJump")
	}
}

func TestUpdateEscClosesJumpWithoutBackingLevel(t *testing.T) {
	m := &Model{
		Spaces: []Space{{Name: "alpha", Services: []Service{{Name: "api"}}}},
		Level:  LevelServices,
	}
	m.OpenJump()
	if !m.Update(KeyBack) {
		t.Fatal("Esc should close the open palette")
	}
	if m.JumpOpen {
		t.Fatal("palette must be closed")
	}
	if m.Level != LevelServices {
		t.Fatalf("closing the palette must not also back out of the drilled-in level, got %v", m.Level)
	}
}
