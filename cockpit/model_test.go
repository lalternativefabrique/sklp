package cockpit

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func threeSpaces() []Space {
	return []Space{
		{Name: "alpha", AllReady: true},
		{Name: "bravo", AllReady: false},
		{Name: "charlie", Err: "boom"},
	}
}

func TestUpdateSelectionWraps(t *testing.T) {
	m := &Model{Spaces: threeSpaces()}

	tests := []struct {
		name  string
		start int
		key   Key
		want  int
	}{
		{"down moves", 0, KeyDown, 1},
		{"up moves", 2, KeyUp, 1},
		{"down wraps at end", 2, KeyDown, 0},
		{"up wraps at start", 0, KeyUp, 2},
		{"j is down", 0, KeyDown, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m.Selected = tt.start
			m.Update(tt.key)
			if m.Selected != tt.want {
				t.Fatalf("Selected=%d want %d", m.Selected, tt.want)
			}
		})
	}
}

func TestUpdateSelectionEmptyList(t *testing.T) {
	m := &Model{}
	if m.Update(KeyDown) {
		t.Fatal("Update on empty list should report no change")
	}
	if m.Selected != 0 {
		t.Fatalf("Selected=%d want 0", m.Selected)
	}
}

func TestUpdateFilterCycles(t *testing.T) {
	m := &Model{}
	want := []Filter{FilterDev, FilterPipe, FilterDeploy, FilterAll}
	for i, w := range want {
		m.Update(KeyFilter)
		if m.Filter != w {
			t.Fatalf("step %d: Filter=%v want %v", i, m.Filter, w)
		}
	}
}

func TestUpdateSortCycles(t *testing.T) {
	m := &Model{Spaces: threeSpaces()}
	m.Update(KeySort)
	if m.Sort != SortState {
		t.Fatalf("Sort=%v want %v", m.Sort, SortState)
	}
	m.Update(KeySort)
	if m.Sort != SortName {
		t.Fatalf("Sort=%v want %v", m.Sort, SortName)
	}
}

func TestSortStateFloatsUnhealthyFirst(t *testing.T) {
	m := &Model{Spaces: threeSpaces(), Sort: SortName}
	m.applySort() // name order: alpha, bravo, charlie
	if m.Spaces[0].Name != "alpha" {
		t.Fatalf("name sort head=%s want alpha", m.Spaces[0].Name)
	}
	m.Sort = SortState
	m.applySort()
	// charlie (err) first, then bravo (not ready), then alpha (ready).
	got := []string{m.Spaces[0].Name, m.Spaces[1].Name, m.Spaces[2].Name}
	want := []string{"charlie", "bravo", "alpha"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("state sort = %v want %v", got, want)
		}
	}
}

func TestApplySortKeepsSelection(t *testing.T) {
	m := &Model{Spaces: threeSpaces(), Selected: 0, Sort: SortName} // alpha selected
	m.Sort = SortState
	m.applySort()
	if sel := m.Selection(); sel == nil || sel.Name != "alpha" {
		t.Fatalf("selection followed wrong space: %+v", sel)
	}
}

func TestFilterHint(t *testing.T) {
	m := &Model{}
	if m.FilterHint() != "" {
		t.Fatal("FilterAll should have no hint")
	}
	m.Filter = FilterDev
	if m.FilterHint() == "" {
		t.Fatal("non-All filter should surface an honest hint")
	}
}

func TestRollupCountsTotalAndSupervised(t *testing.T) {
	spaces := []Space{
		{Name: "alpha", Supervised: true, AllReady: true, Services: []Service{{State: StateReady}, {State: StateBusy}}},
		{Name: "dd", LimitedInfo: true, Status: "running", Containers: 2},
	}
	h := rollup(spaces)
	if h.Spaces != 2 {
		t.Fatalf("Spaces=%d want 2", h.Spaces)
	}
	if h.Supervised != 1 {
		t.Fatalf("Supervised=%d want 1", h.Supervised)
	}
	if h.Services != 2 {
		t.Fatalf("Services=%d want 2 (only supervised contribute services)", h.Services)
	}
	if h.Ready != 1 || h.Busy != 1 {
		t.Fatalf("Ready=%d Busy=%d want 1/1", h.Ready, h.Busy)
	}
}

func TestToggleLogsSupervisedOpensAndCloses(t *testing.T) {
	m := &Model{Spaces: []Space{
		{Name: "alpha", Supervised: true, AllReady: true, Services: []Service{{Name: "api", State: StateReady}}},
	}}
	m.ToggleLogs()
	if !m.LogsOpen {
		t.Fatal("toggle should open the panel for a supervised space")
	}
	if m.LogsService != "api" {
		t.Fatalf("LogsService=%q want api", m.LogsService)
	}
	if m.LogsErr != "" {
		t.Fatalf("supervised space should not set LogsErr, got %q", m.LogsErr)
	}
	m.ToggleLogs()
	if m.LogsOpen {
		t.Fatal("second toggle should close the panel")
	}
}

func TestToggleLogsNonSupervisedSetsErr(t *testing.T) {
	m := &Model{Spaces: []Space{
		{Name: "dd", LimitedInfo: true, Status: "running", Containers: 2},
	}}
	m.ToggleLogs()
	if !m.LogsOpen {
		t.Fatal("panel should open to show the honest message")
	}
	if m.LogsService != "" {
		t.Fatalf("non-supervised must not pick a service, got %q", m.LogsService)
	}
	if m.LogsErr == "" {
		t.Fatal("non-supervised space must set an honest LogsErr")
	}
}

func TestAppendLogLineRingCap(t *testing.T) {
	m := &Model{LogsOpen: true, LogsScroll: -1}
	for i := 0; i < logBufferCap+50; i++ {
		m.AppendLogLine("line")
	}
	if len(m.LogsLines) != logBufferCap {
		t.Fatalf("buffer=%d want cap %d", len(m.LogsLines), logBufferCap)
	}
}

func TestScrollLogsClamps(t *testing.T) {
	// Height 10 => 5 visible lines; a 10-line buffer is taller than the window
	// so scrolling up is meaningful (maxTop = 10-5 = 5).
	m := &Model{LogsOpen: true, LogsScroll: -1, Height: 10}
	for i := 0; i < 10; i++ {
		m.AppendLogLine("l")
	}
	// scroll up off the tail leaves follow mode.
	m.ScrollLogs(-3)
	if m.LogsScroll < 0 {
		t.Fatalf("scroll up should leave follow mode, got %d", m.LogsScroll)
	}
	// scrolling far up clamps to 0.
	m.ScrollLogs(-100)
	if m.LogsScroll != 0 {
		t.Fatalf("scroll clamp top=%d want 0", m.LogsScroll)
	}
	// scrolling back down to the bottom re-enters follow (-1).
	m.ScrollLogs(100)
	if m.LogsScroll != -1 {
		t.Fatalf("scroll to bottom should re-follow, got %d", m.LogsScroll)
	}
}

func TestUpdateRoutesArrowsByPanelState(t *testing.T) {
	m := &Model{Spaces: []Space{
		{Name: "alpha", Supervised: true, Services: []Service{{Name: "api"}}},
		{Name: "bravo", Supervised: true, Services: []Service{{Name: "api"}}},
	}}
	// Closed: down selects.
	m.Update(KeyDown)
	if m.Selected != 1 {
		t.Fatalf("closed panel: down should select, Selected=%d", m.Selected)
	}
	// Open the panel, feed lines, then arrows scroll (selection unchanged).
	m.Selected = 0
	m.Update(KeyLogs)
	if !m.LogsOpen {
		t.Fatal("KeyLogs should open the panel")
	}
	// A buffer larger than the visible window so scrolling up is meaningful
	// (with fewer lines than the window there is nothing above to scroll to).
	m.Height = 10 // logsHeightFor(10) = 5 visible lines
	for i := 0; i < 30; i++ {
		m.AppendLogLine("x")
	}
	m.Update(KeyUp)
	if m.Selected != 0 {
		t.Fatalf("open panel: arrows must not change selection, Selected=%d", m.Selected)
	}
	if m.LogsScroll < 0 {
		t.Fatal("open panel: up should scroll the buffer")
	}
	// KeyLogs again closes.
	m.Update(KeyLogs)
	if m.LogsOpen {
		t.Fatal("KeyLogs should close an open panel")
	}
}

func multiSvcSpace() []Space {
	return []Space{{
		Name: "demo", Supervised: true, AllReady: true,
		Services: []Service{
			{Name: "caddy", State: StateReady},
			{Name: "mysql", State: StateReady},
			{Name: "nginx", State: StateReady},
		},
	}}
}

func TestEnterSpaceSupervisedDrillsIn(t *testing.T) {
	m := &Model{Spaces: multiSvcSpace()}
	if !m.Update(KeyEnter) {
		t.Fatal("Enter on a supervised multi-service space should drill in")
	}
	if m.Level != LevelServices {
		t.Fatalf("Level=%v want LevelServices", m.Level)
	}
	if m.SvcIndex != 0 {
		t.Fatalf("SvcIndex=%d want 0", m.SvcIndex)
	}
	if svc := m.SelectedService(); svc == nil || svc.Name != "caddy" {
		t.Fatalf("SelectedService=%+v want caddy", svc)
	}
}

// A non-supervised space carries container rows read from the runc state, so it
// drills in like any other: the control API is what readiness and logs need, not
// what makes the containers visible.
func TestEnterSpaceNonSupervisedDrillsIntoContainers(t *testing.T) {
	m := &Model{Spaces: []Space{{
		Name: "dd", LimitedInfo: true, Status: "running", Containers: 2,
		Services: []Service{
			{Name: "core", State: StateReady, Container: "sklp-deploy-dd-core"},
			{Name: "migrate", State: StateIdle, Container: "sklp-deploy-dd-migrate"},
		},
	}}}
	if !m.Update(KeyEnter) {
		t.Fatal("Enter on a non-supervised space with containers should drill in")
	}
	if m.Level != LevelServices {
		t.Fatalf("Level=%v want LevelServices", m.Level)
	}
	if svc := m.SelectedService(); svc == nil || svc.Name != "core" {
		t.Fatalf("SelectedService=%+v want core", svc)
	}
}

// An empty space is the only dead end: there is nothing to enter.
func TestEnterSpaceEmptyStaysSpaces(t *testing.T) {
	m := &Model{Spaces: []Space{{Name: "dd", LimitedInfo: true, Status: "stopped"}}}
	if m.Update(KeyEnter) {
		t.Fatal("Enter on an empty space should be a no-op")
	}
	if m.Level != LevelSpaces {
		t.Fatalf("Level=%v want LevelSpaces", m.Level)
	}
	if m.Note == "" {
		t.Fatal("a failed drill-in should set an honest note")
	}
	if m.SelectedService() != nil {
		t.Fatal("SelectedService must be nil at LevelSpaces")
	}
}

func TestServicesLevelArrowsMoveSvcIndexClamped(t *testing.T) {
	m := &Model{Spaces: multiSvcSpace()}
	m.Update(KeyEnter)
	// up at top stays clamped at 0 and does not move space selection.
	m.Update(KeyUp)
	if m.SvcIndex != 0 || m.Selected != 0 {
		t.Fatalf("up clamp: SvcIndex=%d Selected=%d want 0/0", m.SvcIndex, m.Selected)
	}
	m.Update(KeyDown)
	m.Update(KeyDown)
	if m.SvcIndex != 2 {
		t.Fatalf("SvcIndex=%d want 2", m.SvcIndex)
	}
	// down past the end clamps at len-1 and never touches space selection.
	m.Update(KeyDown)
	if m.SvcIndex != 2 {
		t.Fatalf("down clamp: SvcIndex=%d want 2", m.SvcIndex)
	}
	if m.Selected != 0 {
		t.Fatalf("services-level arrows must not move space selection, Selected=%d", m.Selected)
	}
}

func TestBackLevelResetsToSpaces(t *testing.T) {
	m := &Model{Spaces: multiSvcSpace()}
	m.Update(KeyEnter)
	m.Update(KeyDown)
	if !m.Update(KeyBack) {
		t.Fatal("Esc/Back at services level should step back")
	}
	if m.Level != LevelSpaces || m.SvcIndex != 0 {
		t.Fatalf("Back: Level=%v SvcIndex=%d want LevelSpaces/0", m.Level, m.SvcIndex)
	}
	// Back at spaces level is a no-op.
	if m.Update(KeyBack) {
		t.Fatal("Back at spaces level should be a no-op")
	}
}

func TestLogsServicesLevelTargetsHighlighted(t *testing.T) {
	m := &Model{Spaces: multiSvcSpace()}
	m.Update(KeyEnter)
	m.Update(KeyDown) // highlight mysql
	m.Update(KeyLogs)
	if !m.LogsOpen {
		t.Fatal("KeyLogs should open the panel at services level")
	}
	if m.LogsService != "mysql" {
		t.Fatalf("LogsService=%q want mysql (highlighted, not first)", m.LogsService)
	}
}

func TestLogsSpacesLevelTargetsFirst(t *testing.T) {
	m := &Model{Spaces: multiSvcSpace()}
	m.Update(KeyLogs)
	if m.LogsService != "caddy" {
		t.Fatalf("LogsService=%q want caddy (first service at spaces level)", m.LogsService)
	}
}

func TestFlowActive(t *testing.T) {
	if (FlowState{}).Active() {
		t.Fatal("empty flow must be inactive")
	}
	if !(FlowState{Branch: "feat/x"}).Active() {
		t.Fatal("branch present must be active")
	}
}

// killSpaces returns a bare (non-supervised) and a supervised space for kill tests.
func killSpaces() []Space {
	return []Space{
		{Name: "bare", LimitedInfo: true, Status: "running"},
		{Name: "deploy", Supervised: true, AllReady: true},
	}
}

func TestKillSupervisedRefused(t *testing.T) {
	defer func() { KillSpace = nil }()
	called := false
	KillSpace = func(string) error { called = true; return nil }
	m := &Model{Spaces: killSpaces(), Selected: 1}
	m.Update(KeyKill)
	if m.PendingKill != "" {
		t.Fatalf("supervised space must not arm a confirm, PendingKill=%q", m.PendingKill)
	}
	if called {
		t.Fatal("seam must not be called for a supervised space")
	}
	if !contains(m.Note, "supervised") || !contains(m.Note, "deploy delete") {
		t.Fatalf("refuse note missing supervised/deploy delete: %q", m.Note)
	}
}

func TestKillBareArmsConfirm(t *testing.T) {
	defer func() { KillSpace = nil }()
	called := false
	KillSpace = func(string) error { called = true; return nil }
	m := &Model{Spaces: killSpaces(), Selected: 0}
	m.Update(KeyKill)
	if m.PendingKill != "bare" {
		t.Fatalf("PendingKill=%q want bare", m.PendingKill)
	}
	if called {
		t.Fatal("seam must not fire on the first keypress (confirm required)")
	}
}

func TestKillConfirmCallsSeamAndRemoves(t *testing.T) {
	defer func() { KillSpace = nil }()
	var got string
	calls := 0
	KillSpace = func(name string) error { calls++; got = name; return nil }
	m := &Model{Spaces: killSpaces(), Selected: 0}
	m.Update(KeyKill)
	m.Update(KeyConfirm)
	if calls != 1 || got != "bare" {
		t.Fatalf("seam calls=%d name=%q want 1/bare", calls, got)
	}
	if m.PendingKill != "" {
		t.Fatalf("PendingKill must clear after confirm, got %q", m.PendingKill)
	}
	for _, sp := range m.Spaces {
		if sp.Name == "bare" {
			t.Fatal("killed space must be removed from the list")
		}
	}
	if !contains(m.Note, "torn down") {
		t.Fatalf("success note missing: %q", m.Note)
	}
}

func TestKillCancelClears(t *testing.T) {
	defer func() { KillSpace = nil }()
	called := false
	KillSpace = func(string) error { called = true; return nil }
	m := &Model{Spaces: killSpaces(), Selected: 0}
	m.Update(KeyKill)
	m.Update(KeyBack) // n maps to KeyBack
	if m.PendingKill != "" {
		t.Fatalf("cancel must clear PendingKill, got %q", m.PendingKill)
	}
	if called {
		t.Fatal("seam must not be called on cancel")
	}
}

func TestKillIgnoredAtServicesLevel(t *testing.T) {
	defer func() { KillSpace = nil }()
	KillSpace = func(string) error { t.Fatal("seam must not fire at services level"); return nil }
	m := &Model{Spaces: multiSvcSpace()}
	m.Update(KeyEnter) // drill into services
	m.Update(KeyKill)
	if m.PendingKill != "" {
		t.Fatalf("x at services level must be a no-op, PendingKill=%q", m.PendingKill)
	}
}

func TestKillIgnoredWhenLogsOpen(t *testing.T) {
	defer func() { KillSpace = nil }()
	KillSpace = func(string) error { t.Fatal("seam must not fire with logs open"); return nil }
	m := &Model{Spaces: killSpaces(), Selected: 0}
	m.LogsOpen = true
	m.Update(KeyKill)
	if m.PendingKill != "" {
		t.Fatalf("x with logs open must be a no-op, PendingKill=%q", m.PendingKill)
	}
}

func TestKillSeamErrorSurfaced(t *testing.T) {
	defer func() { KillSpace = nil }()
	KillSpace = func(string) error { return errTest }
	m := &Model{Spaces: killSpaces(), Selected: 0}
	m.Update(KeyKill)
	m.Update(KeyConfirm)
	if m.PendingKill != "" {
		t.Fatalf("PendingKill must clear after a failed kill, got %q", m.PendingKill)
	}
	if !contains(m.Note, "boom-kill") {
		t.Fatalf("error note missing the seam error: %q", m.Note)
	}
	// the space must remain since the kill failed.
	found := false
	for _, sp := range m.Spaces {
		if sp.Name == "bare" {
			found = true
		}
	}
	if !found {
		t.Fatal("failed kill must not remove the space")
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

var errTest = errKill("boom-kill")

type errKill string

func (e errKill) Error() string { return string(e) }

func TestHostRunSelectionAndLogs(t *testing.T) {
	CILogStream = func(_ context.Context, _, _ string, _ func(string)) (string, error) { return "", nil }
	defer func() { CILogStream = nil }()

	m := &Model{
		Spaces:   []Space{{Name: "ci-sklp", LimitedInfo: true}},
		HostRuns: []HostRun{{Name: "publish", HasLog: true}, {Name: "deploy", HasLog: false}},
	}
	// Combined list: [ci-sklp, publish, deploy] → 3 rows.
	if got := m.rowCount(); got != 3 {
		t.Fatalf("rowCount = %d, want 3", got)
	}
	// Move down twice lands on the 2nd host-run.
	m.Selected = 0
	m.move(1) // publish
	if hr := m.SelectedHostRun(); hr == nil || hr.Name != "publish" {
		t.Fatalf("after 1 down, SelectedHostRun = %v, want publish", m.SelectedHostRun())
	}
	if m.Selection() != nil {
		t.Fatal("Selection() must be nil on a host-run row")
	}
	// [l] on publish (has a log) opens a CI-log tail keyed by its name.
	m.ToggleLogs()
	if !m.LogsOpen || !m.LogsCI || m.LogsTask != "publish" {
		t.Fatalf("publish logs: open=%v ci=%v task=%q", m.LogsOpen, m.LogsCI, m.LogsTask)
	}
	m.CloseLogs()
	// [l] on deploy (no log) opens with an honest message, not a stream.
	m.move(1) // deploy
	m.ToggleLogs()
	if !m.LogsOpen || m.LogsCI || m.LogsErr == "" {
		t.Fatalf("deploy logs: open=%v ci=%v err=%q", m.LogsOpen, m.LogsCI, m.LogsErr)
	}
}

func TestRenderHostRunsInSpacesTable(t *testing.T) {
	m := &Model{
		Spaces:   []Space{{Name: "ci-sklp", LimitedInfo: true, Status: "running"}},
		HostRuns: []HostRun{{Name: "publish", HasLog: true}},
		Selected: 1, // the host-run row
	}
	out := Render(m, NewPlainRenderer())
	// Host-runs get their own PIPELINE table under the spaces one, sharing the
	// single ↑/↓ selection.
	if !strings.Contains(out, "PIPELINE") {
		t.Fatalf("publish sub-section missing:\n%s", out)
	}
	if !strings.Contains(out, "> [??]  publish") {
		t.Fatalf("selection should land on the host-run row:\n%s", out)
	}
}

func TestRenderHostRuns_NoSubheadingWhenEmpty(t *testing.T) {
	m := &Model{Spaces: []Space{{Name: "x", LimitedInfo: true}}}
	if strings.Contains(Render(m, NewPlainRenderer()), "PIPELINE") {
		t.Fatal("no host-runs → no publish sub-section")
	}
}

// purgeSpaces mixes idle and busy spaces so a purge's advisory count is
// distinguishable from the total.
func purgeSpaceRows() []Space {
	return []Space{
		{Name: "idle-a", Containers: 0},
		{Name: "busy", Containers: 2},
		{Name: "idle-b", Containers: 0},
	}
}

func TestPurgeArmsConfirmWithoutFiring(t *testing.T) {
	defer func() { PurgeSpaces = nil }()
	called := false
	PurgeSpaces = func() ([]string, error) { called = true; return nil, nil }
	m := &Model{Spaces: purgeSpaceRows()}
	m.Update(KeyPurge)
	if !m.PendingPurge {
		t.Fatal("KeyPurge must arm a confirm")
	}
	if called {
		t.Fatal("seam must not fire on the first keypress (confirm required)")
	}
}

func TestPurgeEmptyListRefused(t *testing.T) {
	defer func() { PurgeSpaces = nil }()
	PurgeSpaces = func() ([]string, error) { t.Fatal("seam must not fire with nothing to purge"); return nil, nil }
	m := &Model{}
	m.Update(KeyPurge)
	if m.PendingPurge {
		t.Fatal("an empty space list must not arm a confirm")
	}
	if !contains(m.Note, "no space") {
		t.Fatalf("note=%q want a no-space hint", m.Note)
	}
}

// A space whose only container is stopped counts as busy in the summary yet is
// purgeable to the registry, so a non-zero container count must still arm.
func TestPurgeArmsWhenEveryContainerLooksBusy(t *testing.T) {
	defer func() { PurgeSpaces = nil }()
	PurgeSpaces = func() ([]string, error) { return []string{"stopped-one"}, nil }
	m := &Model{Spaces: []Space{{Name: "stopped-one", Containers: 1}}}
	m.Update(KeyPurge)
	if !m.PendingPurge {
		t.Fatal("a stopped container must not block arming the purge")
	}
	m.Update(KeyConfirm)
	if len(m.Spaces) != 0 {
		t.Fatalf("spaces=%v want the reaped space removed", m.Spaces)
	}
}

func TestPurgeConfirmRemovesOnlyReapedSpaces(t *testing.T) {
	defer func() { PurgeSpaces = nil }()
	calls := 0
	PurgeSpaces = func() ([]string, error) { calls++; return []string{"idle-a", "idle-b"}, nil }
	m := &Model{Spaces: purgeSpaceRows()}
	m.Update(KeyPurge)
	m.Update(KeyConfirm)
	if calls != 1 {
		t.Fatalf("seam calls=%d want 1", calls)
	}
	if len(m.Spaces) != 1 || m.Spaces[0].Name != "busy" {
		t.Fatalf("spaces=%v want only busy left", m.Spaces)
	}
	if m.PendingPurge {
		t.Fatal("confirm must clear the pending state")
	}
	if !contains(m.Note, "purged 2 space(s)") {
		t.Fatalf("note=%q want the reaped count", m.Note)
	}
}

// The registry decides liveness at teardown: a space the cockpit listed as idle
// may have started a container since. Only what the seam reports is dropped.
func TestPurgeKeepsSpacesTheRegistryDeclined(t *testing.T) {
	defer func() { PurgeSpaces = nil }()
	PurgeSpaces = func() ([]string, error) { return []string{"idle-a"}, nil }
	m := &Model{Spaces: purgeSpaceRows()}
	m.Update(KeyPurge)
	m.Update(KeyConfirm)
	if len(m.Spaces) != 2 {
		t.Fatalf("spaces=%v want idle-b kept (registry declined it)", m.Spaces)
	}
}

func TestPurgeCancelledByAnyOtherKey(t *testing.T) {
	defer func() { PurgeSpaces = nil }()
	PurgeSpaces = func() ([]string, error) { t.Fatal("cancelled purge must not fire the seam"); return nil, nil }
	m := &Model{Spaces: purgeSpaceRows()}
	m.Update(KeyPurge)
	m.Update(KeyBack)
	if m.PendingPurge {
		t.Fatal("a non-confirm key must cancel the pending purge")
	}
	if !contains(m.Note, "cancelled") {
		t.Fatalf("note=%q want a cancellation hint", m.Note)
	}
}

func TestPurgeSeamErrorSurfaced(t *testing.T) {
	defer func() { PurgeSpaces = nil }()
	PurgeSpaces = func() ([]string, error) { return nil, errors.New("registry unreachable") }
	m := &Model{Spaces: purgeSpaceRows()}
	m.Update(KeyPurge)
	m.Update(KeyConfirm)
	if len(m.Spaces) != 3 {
		t.Fatalf("spaces=%v want all kept on error", m.Spaces)
	}
	if !contains(m.Note, "registry unreachable") {
		t.Fatalf("note=%q want the seam error", m.Note)
	}
}

// Deleting a space deletes its volumes too, so the prompt must name the data at
// stake — the operator has no undo.
func TestKillPromptNamesVolumes(t *testing.T) {
	m := &Model{
		Spaces:      []Space{{Name: "dev", Volumes: []string{"pgdata", "natsdata"}}},
		PendingKill: "dev",
	}
	warn := killVolumeWarning(m)
	if !contains(warn, "pgdata") || !contains(warn, "natsdata") {
		t.Errorf("prompt must name every volume, got %q", warn)
	}
}

func TestKillPromptSilentWithoutVolumes(t *testing.T) {
	m := &Model{Spaces: []Space{{Name: "dev"}}, PendingKill: "dev"}
	if warn := killVolumeWarning(m); warn != "" {
		t.Errorf("no volumes must add no warning, got %q", warn)
	}
}

// A refresh may drop the row between arming and rendering; the prompt must not
// panic on a name it can no longer resolve.
func TestKillPromptToleratesVanishedRow(t *testing.T) {
	m := &Model{PendingKill: "gone"}
	if warn := killVolumeWarning(m); warn != "" {
		t.Errorf("unknown space must add no warning, got %q", warn)
	}
}

func TestIdleVolumeCountCoversIdleSpacesOnly(t *testing.T) {
	m := &Model{Spaces: []Space{
		{Name: "idle", Containers: 0, Volumes: []string{"a", "b"}},
		{Name: "busy", Containers: 1, Volumes: []string{"c"}},
	}}
	if got := m.idleVolumeCount(); got != 2 {
		t.Errorf("idleVolumeCount() = %d, want 2 (busy space excluded)", got)
	}
}

// The panel cannot know from the space alone whether it got a CI or a dev
// run-log — both live in the same store. It learns from the seam's announce
// line, which is the only carrier of the resolved path.
func TestAppendLogLineCapturesRunLogPath(t *testing.T) {
	m := &Model{}
	m.AppendLogLine("── tailing /cache/sklp/logs/acme/dev-reranker-20260805-095722.log ──")
	if want := "/cache/sklp/logs/acme/dev-reranker-20260805-095722.log"; m.LogsPath != want {
		t.Errorf("LogsPath = %q, want %q", m.LogsPath, want)
	}
	// Later lines must not overwrite it, even one that looks like an announce.
	m.AppendLogLine("── tailing /elsewhere/ci-other-20260805-100000.log ──")
	if !strings.HasSuffix(m.LogsPath, "dev-reranker-20260805-095722.log") {
		t.Errorf("LogsPath overwritten by a later line: %q", m.LogsPath)
	}
	// A plain service log line leaves the path empty: no announce, no capture.
	m2 := &Model{}
	m2.AppendLogLine("[tei-reranker] Ready")
	if m2.LogsPath != "" {
		t.Errorf("LogsPath = %q on a non-announce line, want empty", m2.LogsPath)
	}
}

// Copying must hand back the buffer lines, not what the pane displays: the pane
// hard-wraps to its width, and those fragments are exactly what the operator
// cannot use.
func TestCopyLogsCopiesUnwrappedBuffer(t *testing.T) {
	var got string
	Clipboard = func(s string) error { got = s; return nil }
	defer func() { Clipboard = nil }()

	long := strings.Repeat("x", 250)
	m := &Model{LogsOpen: true, Width: 100, Height: 40, LogsLines: []string{long, "short"}}
	if !m.Update(KeyCopy) {
		t.Fatal("copy must report a model change so the note is drawn")
	}
	if want := long + "\nshort\n"; got != want {
		t.Errorf("clipboard got %q, want the raw buffer %q", got, want)
	}
	if !strings.Contains(m.LogsNote, "2 line(s) copied") {
		t.Errorf("the note must report what was copied: %q", m.LogsNote)
	}
}

func TestCopyLogsWithoutClipboardToolIsHonest(t *testing.T) {
	Clipboard = nil
	m := &Model{LogsOpen: true, LogsLines: []string{"line"}}
	m.Update(KeyCopy)
	if m.LogsNote == "" {
		t.Error("a missing clipboard seam must be reported, not silently swallowed")
	}
}

func TestCopyLogsSurfacesTheFailure(t *testing.T) {
	Clipboard = func(string) error { return errors.New("wl-copy: no display") }
	defer func() { Clipboard = nil }()
	m := &Model{LogsOpen: true, LogsLines: []string{"line"}}
	m.Update(KeyCopy)
	if !strings.Contains(m.LogsNote, "no display") {
		t.Errorf("the failure reason must reach the operator: %q", m.LogsNote)
	}
}

func TestCopyLogsEmptyBuffer(t *testing.T) {
	called := false
	Clipboard = func(string) error { called = true; return nil }
	defer func() { Clipboard = nil }()
	m := &Model{LogsOpen: true}
	m.Update(KeyCopy)
	if called {
		t.Error("an empty buffer must not reach the clipboard")
	}
	if m.LogsNote == "" {
		t.Error("the operator must learn there was nothing to copy")
	}
}

// Copy is a logs-panel action; at the spaces level c must stay inert rather than
// copy a stale buffer.
func TestCopyIsANoOpWhenThePanelIsClosed(t *testing.T) {
	Clipboard = func(string) error { t.Fatal("closed panel must not copy"); return nil }
	defer func() { Clipboard = nil }()
	m := &Model{Spaces: []Space{{Name: "alpha"}}}
	if m.Update(KeyCopy) {
		t.Error("KeyCopy at spaces level must not change the model")
	}
}

// Reopening the panel must not carry the previous copy report over.
func TestLogsNoteClearedOnReopen(t *testing.T) {
	m := &Model{Spaces: []Space{{Name: "alpha", LimitedInfo: true}}, LogsNote: "stale"}
	m.ToggleLogs()
	if m.LogsNote != "" {
		t.Errorf("a reopened panel must start without a stale note: %q", m.LogsNote)
	}
}

// Scrolling counted RAW buffer lines while the panel windows on WRAPPED rows, so
// with long log lines maxTop was a fraction of the real row count: two presses
// hit the bottom and snapped back to follow. One press must advance one row.
func TestScrollLogsCountsWrappedRows(t *testing.T) {
	m := &Model{LogsOpen: true, LogsScroll: -1, Width: 200, Height: 20}
	width := logsTextWidth(m)
	// Each line is 3 screen rows wide.
	for i := 0; i < 10; i++ {
		m.AppendLogLine(strings.Repeat("x", width*3))
	}
	rows := len(m.logRows())
	if rows != 30 {
		t.Fatalf("10 lines of 3 rows = 30 screen rows, got %d", rows)
	}
	height := logsHeight(m.Height, logsPaneExtraRows(m))
	wantTop := rows - height

	m.ScrollLogs(-1)
	if got := m.LogsScroll; got != wantTop-1 {
		t.Errorf("one press up must move exactly one screen row: top=%d want %d", got, wantTop-1)
	}
	m.ScrollLogs(-100)
	if m.LogsScroll != 0 {
		t.Errorf("scrolling far up must clamp to the oldest row, got %d", m.LogsScroll)
	}
	// From the very top the tail is still a long way down: a single press must NOT
	// snap back to follow, which is what the raw-line count used to do.
	if m.ScrollLogs(+1); m.LogsScroll != 1 {
		t.Errorf("one press down from the top must land on row 1, got %d", m.LogsScroll)
	}
}

func TestLogsPagingAndJumps(t *testing.T) {
	m := &Model{LogsOpen: true, LogsScroll: -1, Width: 200, Height: 20}
	for i := 0; i < 100; i++ {
		m.AppendLogLine("line")
	}
	page := m.logsPage()
	if page < 2 {
		t.Fatalf("a page must be more than one row, got %d", page)
	}
	top := len(m.logRows()) - logsHeight(m.Height, logsPaneExtraRows(m))

	if !m.Update(KeyPageUp) {
		t.Fatal("PgUp must scroll")
	}
	if got := m.LogsScroll; got != top-page {
		t.Errorf("PgUp must move one page: top=%d want %d", got, top-page)
	}
	if !m.Update(KeyHome) {
		t.Fatal("g must jump to the top")
	}
	if m.LogsScroll != 0 {
		t.Errorf("g must pin the oldest row, got %d", m.LogsScroll)
	}
	if !m.Update(KeyPageDown) {
		t.Fatal("PgDn must scroll")
	}
	if got := m.LogsScroll; got != page {
		t.Errorf("PgDn from the top must land one page down: got %d want %d", got, page)
	}
	if !m.Update(KeyEnd) {
		t.Fatal("G must return to follow")
	}
	if m.LogsScroll != -1 {
		t.Errorf("G must re-enter follow mode, got %d", m.LogsScroll)
	}
	if m.Update(KeyEnd) {
		t.Error("G while already following must not report a change")
	}
}

// The scroll keys belong to the panel; at spaces level they must not fire.
func TestPagingIsInertWhenPanelClosed(t *testing.T) {
	m := &Model{Spaces: []Space{{Name: "alpha"}}}
	for _, k := range []Key{KeyPageUp, KeyPageDown, KeyHome, KeyEnd} {
		if m.Update(k) {
			t.Errorf("key %v must be inert with the logs panel closed", k)
		}
	}
}

// A stop must keep the row listed: that is what separates it from a delete. The
// old TUI only offered Down, which destroys the named volumes.
func TestRequestStopKeepsTheSpaceListed(t *testing.T) {
	var stopped string
	StopSpace = func(name string) error { stopped = name; return nil }
	defer func() { StopSpace = nil }()

	m := &Model{Spaces: []Space{{
		Name: "sklp-dev-71ed", LimitedInfo: true, Status: "running",
		Containers: 5, Volumes: []string{"pgdata"},
	}}}
	if !m.Update(KeyStop) {
		t.Fatal("stop must change the model")
	}
	if stopped != "sklp-dev-71ed" {
		t.Errorf("the seam got %q, want the selected space", stopped)
	}
	if len(m.Spaces) != 1 {
		t.Fatalf("a stopped space must stay listed, got %d rows", len(m.Spaces))
	}
	sp := m.Spaces[0]
	if sp.Status != statusStopped || sp.Containers != 0 {
		t.Errorf("the row must read as stopped, got status=%q containers=%d", sp.Status, sp.Containers)
	}
	if len(sp.Volumes) != 1 {
		t.Error("a stop must not drop the volumes from the row — the data is still there")
	}
	if !strings.Contains(m.Note, "volumes kept") {
		t.Errorf("the note must say the data survived: %q", m.Note)
	}
}

// Stop is not destructive and `up` undoes it, so it must act immediately rather
// than arm a confirm the way a delete does.
func TestRequestStopNeedsNoConfirmation(t *testing.T) {
	StopSpace = func(string) error { return nil }
	defer func() { StopSpace = nil }()
	m := &Model{Spaces: []Space{{Name: "alpha", LimitedInfo: true, Status: "running"}}}
	m.Update(KeyStop)
	if m.PendingKill != "" || m.PendingPurge {
		t.Error("a stop must not arm a destructive confirm")
	}
}

func TestRequestStopRejectsAlreadyStopped(t *testing.T) {
	StopSpace = func(string) error { t.Fatal("must not stop an already stopped space"); return nil }
	defer func() { StopSpace = nil }()
	m := &Model{Spaces: []Space{{Name: "alpha", LimitedInfo: true, Status: statusStopped}}}
	m.Update(KeyStop)
	if !strings.Contains(m.Note, "already stopped") {
		t.Errorf("the operator must learn why nothing happened: %q", m.Note)
	}
}

func TestRequestStopRejectsSupervisedDeploy(t *testing.T) {
	StopSpace = func(string) error { t.Fatal("a supervised deploy is not stopped from the TUI"); return nil }
	defer func() { StopSpace = nil }()
	m := &Model{Spaces: []Space{{Name: "prod", Supervised: true, Status: "running"}}}
	m.Update(KeyStop)
	if !strings.Contains(m.Note, "supervised deploy") {
		t.Errorf("the note must point at the right command: %q", m.Note)
	}
}

func TestRequestStopSurfacesFailure(t *testing.T) {
	StopSpace = func(string) error { return errors.New("runc: permission denied") }
	defer func() { StopSpace = nil }()
	m := &Model{Spaces: []Space{{Name: "alpha", LimitedInfo: true, Status: "running"}}}
	m.Update(KeyStop)
	if !strings.Contains(m.Note, "permission denied") {
		t.Errorf("the failure must reach the operator: %q", m.Note)
	}
	if m.Spaces[0].Status == statusStopped {
		t.Error("a failed stop must not report the space as stopped")
	}
}

// Stop acts on a space; with the logs panel open s belongs to the panel's own
// bindings, and at services level there is no space row to act on.
func TestRequestStopIsInertOutsideTheSpacesList(t *testing.T) {
	StopSpace = func(string) error { t.Fatal("must not stop"); return nil }
	defer func() { StopSpace = nil }()
	m := &Model{Spaces: []Space{{Name: "alpha", LimitedInfo: true, Status: "running"}}, LogsOpen: true}
	m.Update(KeyStop)
	m2 := &Model{Spaces: []Space{{Name: "alpha", LimitedInfo: true, Status: "running"}}, Level: LevelServices}
	m2.Update(KeyStop)
}

// Enter is the reflex for answering a prompt. It used to fall through to the
// cancel path, so reaching for it silently aborted the delete.
func TestEnterConfirmsAPendingKill(t *testing.T) {
	var killed string
	KillSpace = func(name string) error { killed = name; return nil }
	defer func() { KillSpace = nil }()

	m := &Model{Spaces: []Space{{Name: "doomed", LimitedInfo: true}}}
	m.Update(KeyKill)
	if m.PendingKill != "doomed" {
		t.Fatalf("kill should be armed, got %q", m.PendingKill)
	}
	if !m.Update(KeyEnter) {
		t.Fatal("Enter must answer the prompt")
	}
	if killed != "doomed" {
		t.Errorf("Enter must confirm the kill, seam got %q", killed)
	}
	if m.PendingKill != "" {
		t.Error("the prompt must be cleared once answered")
	}
}

func TestEnterConfirmsAPendingPurge(t *testing.T) {
	called := false
	PurgeSpaces = func() ([]string, error) { called = true; return []string{"a"}, nil }
	defer func() { PurgeSpaces = nil }()

	m := &Model{Spaces: []Space{{Name: "a", LimitedInfo: true}}}
	m.Update(KeyPurge)
	if !m.PendingPurge {
		t.Fatal("purge should be armed")
	}
	m.Update(KeyEnter)
	if !called {
		t.Error("Enter must confirm the purge")
	}
}

// While a prompt is armed Enter answers it and must NOT also drill into the
// selected space.
func TestEnterDoesNotDrillInWhileConfirming(t *testing.T) {
	KillSpace = func(string) error { return nil }
	defer func() { KillSpace = nil }()
	m := &Model{Spaces: []Space{{
		Name: "doomed", Supervised: false, LimitedInfo: true,
		Services: []Service{{Name: "web"}},
	}}}
	m.Update(KeyKill)
	m.Update(KeyEnter)
	if m.Level != LevelSpaces {
		t.Error("answering a prompt must not also drill into the space")
	}
}

// n and Esc must still cancel — accepting Enter must not turn every key into a yes.
func TestCancelKeysStillCancel(t *testing.T) {
	KillSpace = func(string) error { t.Fatal("must not kill on a cancel"); return nil }
	defer func() { KillSpace = nil }()
	for _, k := range []Key{KeyBack, KeyUp} {
		m := &Model{Spaces: []Space{{Name: "doomed", LimitedInfo: true}}}
		m.Update(KeyKill)
		m.Update(k)
		if m.PendingKill != "" {
			t.Errorf("key %v must clear the prompt", k)
		}
		if !strings.Contains(m.Note, "cancelled") {
			t.Errorf("key %v must report the cancel: %q", k, m.Note)
		}
	}
}
