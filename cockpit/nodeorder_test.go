package cockpit

import (
	"context"
	"testing"
	"time"
)

func mixedNodeSpaces() []Space {
	return []Space{
		{Name: "zulu", Node: ""},
		{Name: "alpha", Node: "prod"},
		{Name: "mike", Node: ""},
		{Name: "bravo", Node: "ci"},
	}
}

// Sorting must group by node so the flat slice order matches the per-node
// tables the view renders; otherwise ↑/↓ jumps between tables.
func TestSortSpacesGroupsByNodeLocalFirst(t *testing.T) {
	for _, mode := range []Sort{SortName, SortState} {
		t.Run(mode.String(), func(t *testing.T) {
			spaces := mixedNodeSpaces()
			sortSpaces(spaces, mode)

			want := []struct{ name, node string }{
				{"mike", ""},
				{"zulu", ""},
				{"bravo", "ci"},
				{"alpha", "prod"},
			}
			if len(spaces) != len(want) {
				t.Fatalf("len=%d want %d", len(spaces), len(want))
			}
			for i, w := range want {
				if spaces[i].Name != w.name || spaces[i].Node != w.node {
					t.Fatalf("row %d = %s/%s want %s/%s", i, spaces[i].Name, spaces[i].Node, w.name, w.node)
				}
			}
		})
	}
}

// Walking the list with ↓ must visit rows in the same order the view draws
// them: every group contiguous, never bouncing back to an earlier node.
func TestMoveFollowsRenderedGroupOrder(t *testing.T) {
	spaces := mixedNodeSpaces()
	sortSpaces(spaces, SortName)
	m := &Model{Spaces: spaces}

	var walked []string
	for i := 0; i < len(spaces); i++ {
		walked = append(walked, m.Spaces[m.Selected].Node)
		m.Update(KeyDown)
	}

	seen := map[string]bool{}
	prev := "\x00"
	for _, node := range walked {
		if node != prev {
			if seen[node] {
				t.Fatalf("node %q revisited after leaving it: %v", node, walked)
			}
			seen[node] = true
			prev = node
		}
	}

	groups := groupByNode(m.Spaces)
	for _, g := range groups {
		for i, nr := range g.rows {
			if i > 0 && nr.index != g.rows[i-1].index+1 {
				t.Fatalf("node %q rows not contiguous: %d after %d", g.node, nr.index, g.rows[i-1].index)
			}
		}
	}
}

// ApplySnapshot must fold a snapshot exactly as the blocking Refresh did,
// keeping the selection anchored on the same space by name.
func TestApplySnapshotPreservesSelectionByName(t *testing.T) {
	m := &Model{Spaces: mixedNodeSpaces()}
	sortSpaces(m.Spaces, m.Sort)
	for i := range m.Spaces {
		if m.Spaces[i].Name == "bravo" {
			m.Selected = i
		}
	}

	snap := Snapshot{Spaces: []Space{
		{Name: "alpha", Node: "prod"},
		{Name: "bravo", Node: "ci"},
		{Name: "mike", Node: ""},
	}}
	m.ApplySnapshot(snap)

	sel := m.Selection()
	if sel == nil || sel.Name != "bravo" {
		t.Fatalf("selection = %v want bravo", sel)
	}
}

// A vanished space must clamp the selection rather than leave it dangling.
func TestApplySnapshotClampsVanishedSelection(t *testing.T) {
	m := &Model{Spaces: mixedNodeSpaces(), Selected: 3}
	m.ApplySnapshot(Snapshot{Spaces: []Space{{Name: "solo"}}})
	if m.Selected != 0 {
		t.Fatalf("Selected=%d want 0", m.Selected)
	}
}

// ReadSnapshot does the blocking work; it must not touch a Model, so it stays
// safe to call from a goroutine while the UI draws.
func TestReadSnapshotIsPure(t *testing.T) {
	restore := SpaceList
	SpaceList = func() []SpaceSummary { return nil }
	defer func() { SpaceList = restore }()

	if got := ReadSnapshot(context.Background(), LocalContext); got.Spaces != nil && len(got.Spaces) != 0 {
		t.Fatalf("unexpected spaces: %v", got.Spaces)
	}
}

// A slow source must not stall key handling: the tick only schedules the read,
// so Update returns immediately and the snapshot lands later as its own message.
func TestTickDoesNotBlockOnSlowSource(t *testing.T) {
	restore := SpaceList
	SpaceList = func() []SpaceSummary {
		time.Sleep(200 * time.Millisecond)
		return nil
	}
	defer func() { SpaceList = restore }()

	tm := &teaModel{m: &Model{Spaces: threeSpaces()}, r: nil}

	start := time.Now()
	_, cmd := tm.Update(tickMsg{})
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("tick blocked the event loop for %s", elapsed)
	}
	if cmd == nil {
		t.Fatal("tick returned no read command")
	}

	msg := cmd()
	if _, ok := msg.(snapshotMsg); !ok {
		t.Fatalf("got %T want snapshotMsg", msg)
	}
}

// Keys stay responsive while a read is in flight.
func TestKeysHandledDuringSlowRead(t *testing.T) {
	restore := SpaceList
	release := make(chan struct{})
	done := make(chan struct{})
	SpaceList = func() []SpaceSummary { <-release; return nil }
	// The read goroutine reads the seam, so it must finish before it is restored.
	defer func() { close(release); <-done; SpaceList = restore }()

	tm := &teaModel{m: &Model{Spaces: threeSpaces()}, r: nil}
	_, cmd := tm.Update(tickMsg{})

	go func() { defer close(done); cmd() }()

	start := time.Now()
	tm.m.Update(KeyDown)
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("key handling blocked for %s", elapsed)
	}
	if tm.m.Selected != 1 {
		t.Fatalf("Selected=%d want 1", tm.m.Selected)
	}
}
