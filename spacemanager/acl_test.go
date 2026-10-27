package spacemanager

import (
	"sync"
	"testing"
)

// aclLauncher is an execLauncher that also implements aclApplier, so the
// agent's ACL push is asserted without a real space netns. It records every
// SetACLEdges call.
type aclLauncher struct {
	*execLauncher
	mu    sync.Mutex
	calls [][]ACLEdge
}

func newACLLauncher() *aclLauncher {
	return &aclLauncher{execLauncher: newExecLauncher()}
}

func (l *aclLauncher) SetACLEdges(edges []ACLEdge) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, append([]ACLEdge(nil), edges...))
	return nil
}

func (l *aclLauncher) lastCall() ([]ACLEdge, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.calls) == 0 {
		return nil, false
	}
	return l.calls[len(l.calls)-1], true
}

func (l *aclLauncher) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.calls)
}

// TestSyncACLBuildsGraphFromDependsOn asserts syncACLLocked collects every
// service's DependsOn into (caller, target) edges and pushes the WHOLE graph
// in one call — the same whole-set contract as gatewayRouteLocked/SetRoutes,
// not a per-service delta.
func TestSyncACLBuildsGraphFromDependsOn(t *testing.T) {
	l := newACLLauncher()
	desired := []Service{
		{Name: "app-x", DependsOn: []string{"nats"}},
		{Name: "app-y", DependsOn: []string{"nats"}},
		{Name: "nats"},
	}
	a := NewAgent(l, nil, desired, testOpts())

	a.mu.Lock()
	a.syncACLLocked()
	a.mu.Unlock()

	edges, ok := l.lastCall()
	if !ok {
		t.Fatal("SetACLEdges was never called")
	}
	want := map[ACLEdge]bool{
		{From: "app-x", To: "nats"}: true,
		{From: "app-y", To: "nats"}: true,
	}
	if len(edges) != len(want) {
		t.Fatalf("edges = %v, want exactly %v", edges, want)
	}
	for _, e := range edges {
		if !want[e] {
			t.Errorf("unexpected edge %v not declared by any service's depends_on", e)
		}
	}
	// app-y is never a target of any edge: the isolation guarantee the whole
	// feature exists for. Nothing here should imply app-x can reach app-y.
	for _, e := range edges {
		if e.To == "app-y" {
			t.Fatalf("app-y appears as an edge target despite no service declaring it: %v", edges)
		}
	}
}

// TestSyncACLPushesEmptyWhenNoDependsOn asserts a Space that declares no
// depends_on graph still gets an (empty) SetACLEdges push — not a skipped
// call — so a re-apply that REMOVES a depends_on graph reopens east-west via
// the same call path, rather than relying on the caller to notice and clear
// it separately.
func TestSyncACLPushesEmptyWhenNoDependsOn(t *testing.T) {
	l := newACLLauncher()
	desired := []Service{{Name: "app-x"}, {Name: "nats"}}
	a := NewAgent(l, nil, desired, testOpts())

	a.mu.Lock()
	a.syncACLLocked()
	a.mu.Unlock()

	edges, ok := l.lastCall()
	if !ok {
		t.Fatal("SetACLEdges was never called")
	}
	if len(edges) != 0 {
		t.Fatalf("edges = %v, want empty (no service declares depends_on)", edges)
	}
}

// TestSyncACLNoopWithoutLauncherSupport asserts a launcher that does NOT
// implement aclApplier (the plain exec launcher) leaves the agent's ACL push
// inert: syncACLLocked must not panic and must call nothing, so the exec/test
// path needs no real netns.
func TestSyncACLNoopWithoutLauncherSupport(t *testing.T) {
	l := newExecLauncher()
	desired := []Service{{Name: "app-x", DependsOn: []string{"nats"}}, {Name: "nats"}}
	a := NewAgent(l, nil, desired, testOpts())

	a.mu.Lock()
	a.syncACLLocked() // must not panic
	a.mu.Unlock()
}
