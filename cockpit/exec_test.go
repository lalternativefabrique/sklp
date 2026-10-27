package cockpit

import (
	"errors"
	"strings"
	"testing"
)

// The container rows of a non-supervised space come from the runc state. The ids
// are prefixed with the space's content hash, so the row must carry the readable
// service name while keeping the full id as the exec target.
func TestContainerServicesDeriveNameAndTarget(t *testing.T) {
	info := SpaceSummary{
		Name: "sklp-dev-71ed3254bb55",
		Runc: []ContainerSummary{
			{ID: "sklp-deploy-sklp-dev-71ed3254bb55-core", Status: "running"},
			{ID: "sklp-deploy-sklp-dev-71ed3254bb55-migrate", Status: "stopped"},
		},
	}
	got := containerServices(info)
	if len(got) != 2 {
		t.Fatalf("got %d services, want 2", len(got))
	}
	if got[0].Name != "core" {
		t.Errorf("name = %q, want core (the id prefix must be trimmed)", got[0].Name)
	}
	if got[0].Container != "sklp-deploy-sklp-dev-71ed3254bb55-core" {
		t.Errorf("container = %q, want the full runc id", got[0].Container)
	}
	if got[0].State != StateReady {
		t.Errorf("running container State=%v want StateReady", got[0].State)
	}
	// A converged oneshot is idle, not an error: it did its job and exited.
	if got[1].State != StateIdle {
		t.Errorf("stopped container State=%v want StateIdle", got[1].State)
	}
}

// An id that does not carry the expected prefix is shown whole rather than
// mangled by a blind trim.
func TestServiceNameFromIDLeavesForeignIDsAlone(t *testing.T) {
	if got := serviceNameFromID("weird-id", "dd"); got != "weird-id" {
		t.Errorf("got %q, want the id unchanged", got)
	}
}

// [e] at spaces level has no unambiguous target in a multi-container space: it
// must say so rather than pick one.
func TestExecAtSpacesLevelAsksToDrillIn(t *testing.T) {
	restore := stubSpaceExec(func(string, string, string) error { return nil })
	defer restore()

	m := &Model{Spaces: []Space{{Name: "dd", LimitedInfo: true, Services: []Service{
		{Name: "core", State: StateReady, Container: "sklp-deploy-dd-core"},
	}}}}
	m.Update(KeyExec)
	if m.PendingExec != nil {
		t.Fatal("no shell may be armed before a container is picked")
	}
	if !strings.Contains(m.Note, "↵") {
		t.Errorf("the note must point at the drill-in: %q", m.Note)
	}
}

// [e] on a running container arms a shell aimed at its runc id.
func TestExecArmsShellForHighlightedContainer(t *testing.T) {
	restore := stubSpaceExec(func(string, string, string) error { return nil })
	defer restore()

	m := &Model{Spaces: []Space{{Name: "dd", LimitedInfo: true, Services: []Service{
		{Name: "core", State: StateReady, Container: "sklp-deploy-dd-core"},
	}}}}
	m.Update(KeyEnter)
	m.Update(KeyExec)

	req := m.TakePendingExec()
	if req == nil {
		t.Fatal("[e] on a running container must arm a shell")
	}
	if req.Space != "dd" || req.Container != "sklp-deploy-dd-core" {
		t.Errorf("req = %+v, want space dd / container sklp-deploy-dd-core", req)
	}
	if req.Label != "core" {
		t.Errorf("label = %q, want the readable name", req.Label)
	}
	// Taking the request disarms it: one keypress opens exactly one shell.
	if m.TakePendingExec() != nil {
		t.Error("the request must be consumed once")
	}
}

// A supervised row has no runc id; the service name is the target the registry
// resolves.
func TestExecOnSupervisedRowTargetsServiceName(t *testing.T) {
	restore := stubSpaceExec(func(string, string, string) error { return nil })
	defer restore()

	m := &Model{Spaces: multiSvcSpace()}
	m.Update(KeyEnter)
	m.Update(KeyExec)

	req := m.TakePendingExec()
	if req == nil || req.Container != "caddy" {
		t.Fatalf("req = %+v, want container caddy", req)
	}
}

// A stopped oneshot has no process to enter. Arming a shell into it would fail
// deep in runc, so it is refused up front with a reason.
func TestExecOnStoppedContainerIsRefused(t *testing.T) {
	restore := stubSpaceExec(func(string, string, string) error { return nil })
	defer restore()

	m := &Model{Spaces: []Space{{Name: "dd", LimitedInfo: true, Services: []Service{
		{Name: "migrate", State: StateIdle, Container: "sklp-deploy-dd-migrate"},
	}}}}
	m.Update(KeyEnter)
	m.Update(KeyExec)

	if m.PendingExec != nil {
		t.Fatal("a stopped container must not arm a shell")
	}
	if !strings.Contains(m.Note, "migrate") {
		t.Errorf("the note must name the refused container: %q", m.Note)
	}
}

// With no seam injected the action reports it is unavailable instead of arming a
// request nothing can honour.
func TestExecWithoutSeamIsHonest(t *testing.T) {
	restore := stubSpaceExec(nil)
	defer restore()

	m := &Model{Spaces: []Space{{Name: "dd", LimitedInfo: true, Services: []Service{
		{Name: "core", State: StateReady, Container: "sklp-deploy-dd-core"},
	}}}}
	m.Update(KeyEnter)
	m.Update(KeyExec)

	if m.PendingExec != nil {
		t.Fatal("no seam must mean no armed request")
	}
	if !strings.Contains(m.Note, "unavailable") {
		t.Errorf("note = %q, want an unavailable notice", m.Note)
	}
}

// The logs panel owns the screen and the keyboard; [e] must not fire underneath
// it and steal the terminal from the stream.
func TestExecIsInertWhileLogsPanelOpen(t *testing.T) {
	restore := stubSpaceExec(func(string, string, string) error { return nil })
	defer restore()

	m := &Model{
		LogsOpen: true,
		Spaces: []Space{{Name: "dd", LimitedInfo: true, Services: []Service{
			{Name: "core", State: StateReady, Container: "sklp-deploy-dd-core"},
		}}},
	}
	m.Level = LevelServices
	m.Update(KeyExec)
	if m.PendingExec != nil {
		t.Fatal("[e] must be inert while the logs panel is open")
	}
}

// errStub proves the seam's error reaches the caller unchanged.
var errStub = errors.New("boom")

func TestExecSeamErrorSurfaces(t *testing.T) {
	restore := stubSpaceExec(func(string, string, string) error { return errStub })
	defer restore()

	if err := SpaceExec("", "dd", "core"); !errors.Is(err, errStub) {
		t.Errorf("err = %v, want the seam's own error", err)
	}
}

// stubSpaceExec swaps the package-level seam and returns its restorer, so tests
// never leak a stub into one another.
func stubSpaceExec(fn func(string, string, string) error) func() {
	prev := SpaceExec
	SpaceExec = fn
	return func() { SpaceExec = prev }
}

// A deploy names its containers `<svc>-<instance>`, but the control API keys
// logs and restarts by service name: a row named `core-0` 404s there. Replicas
// of one service collapse onto a single row carrying the instance count.
func TestContainerServicesDropInstanceSuffix(t *testing.T) {
	info := SpaceSummary{
		Name: "production",
		Runc: []ContainerSummary{
			{ID: "sklp-deploy-production-core-0", Status: "running"},
			{ID: "sklp-deploy-production-core-1", Status: "running"},
			{ID: "sklp-deploy-production-web-0", Status: "running"},
		},
	}
	got := containerServices(info)
	if len(got) != 2 {
		t.Fatalf("got %d services, want 2 (core + web)", len(got))
	}
	if got[0].Name != "core" || got[0].Instances != 2 {
		t.Errorf("got %q x%d, want core x2", got[0].Name, got[0].Instances)
	}
	if got[1].Name != "web" || got[1].Instances != 1 {
		t.Errorf("got %q x%d, want web x1", got[1].Name, got[1].Instances)
	}
}

// A remote space whose node answered with its supervised state is NOT limited:
// the rows carry the control API's own service names and readiness, and each
// keeps a runc id so [e] still has a target.
func TestLimitedSpacePromotedBySupervisedState(t *testing.T) {
	info := SpaceSummary{
		Name:       "production",
		Status:     "running",
		Supervised: true,
		Runc: []ContainerSummary{
			{ID: "sklp-deploy-production-core-0", Status: "running"},
			{ID: "sklp-deploy-production-web-0", Status: "running"},
		},
		Services: []ServiceSummary{
			{Name: "core", State: "ready", Instances: 1},
			{Name: "web", State: "ready", Instances: 1},
		},
		AllReady: true,
	}
	sp := limitedSpace("shop-test", info)
	if sp.LimitedInfo {
		t.Error("LimitedInfo = true, want false: the node served its supervised state")
	}
	if !sp.AllReady {
		t.Error("AllReady = false, want true")
	}
	if len(sp.Services) != 2 || sp.Services[0].Name != "core" {
		t.Fatalf("services = %+v, want the control API's own list", sp.Services)
	}
	if sp.Services[0].Container != "sklp-deploy-production-core-0" {
		t.Errorf("container = %q, want the runc id so [e] has a target", sp.Services[0].Container)
	}
	if sp.Services[0].State != StateReady {
		t.Errorf("state = %v, want StateReady", sp.Services[0].State)
	}
}

// Without a supervised answer the row stays container-level, as before.
func TestLimitedSpaceStaysLimitedWithoutState(t *testing.T) {
	sp := limitedSpace("shop-test", SpaceSummary{
		Name:       "production",
		Supervised: true,
		Runc:       []ContainerSummary{{ID: "sklp-deploy-production-core-0", Status: "running"}},
	})
	if !sp.LimitedInfo {
		t.Error("LimitedInfo = false, want true: only the inventory was readable")
	}
}

// A space on another node is reachable: the cockpit pilots it through the node's
// control API, so [e] must arm rather than refuse. The target is the SERVICE
// name — a runc id read from a local row names nothing on the far machine.
func TestExecOnRemoteSpaceArmsWithNode(t *testing.T) {
	restore := stubSpaceExec(func(string, string, string) error { return nil })
	defer restore()

	m := &Model{Spaces: []Space{{Name: "acme-production", Node: "node-b", Services: []Service{
		{Name: "core", State: StateReady, Container: "sklp-deploy-acme-production-core-0"},
	}}}}
	m.Level = LevelServices
	m.Update(KeyExec)
	if m.PendingExec == nil {
		t.Fatalf("[e] must arm on a remote space, got note %q", m.Note)
	}
	if m.PendingExec.Node != "node-b" {
		t.Errorf("Node = %q, want the space's node", m.PendingExec.Node)
	}
	if m.PendingExec.Container != "core" {
		t.Errorf("Container = %q, want the service name for a remote target", m.PendingExec.Container)
	}
}

// The local path keeps addressing the runc id: the embedded runtime resolves an
// id, and collapsing to the service name would lose the replica the row names.
func TestExecOnLocalSpaceKeepsContainerID(t *testing.T) {
	restore := stubSpaceExec(func(string, string, string) error { return nil })
	defer restore()

	m := &Model{Spaces: []Space{{Name: "dd", Services: []Service{
		{Name: "core", State: StateReady, Container: "sklp-dev-dd-core-0"},
	}}}}
	m.Level = LevelServices
	m.Update(KeyExec)
	if m.PendingExec == nil {
		t.Fatal("[e] must arm on a local space")
	}
	if m.PendingExec.Node != "" {
		t.Errorf("Node = %q, want empty for a local space", m.PendingExec.Node)
	}
	if m.PendingExec.Container != "sklp-dev-dd-core-0" {
		t.Errorf("Container = %q, want the runc id", m.PendingExec.Container)
	}
}
