package domain

import (
	"testing"
	"time"
)

func nodeIDs() map[string]string { return map[string]string{"ci-instance-v1-server": "n1"} }

// The incident this fixes: stopping acme-production removed it from the list
// entirely, leaving no way back to it from the UI. Declared in Git means listed.
func TestMergeDeclared_StoppedSpaceStaysListed(t *testing.T) {
	nodes := []Node{{Name: "ci-instance-v1-server", Spaces: []Space{{Name: "shop-production", Status: "running"}}}}
	declared := []DeclaredSpace{
		{Space: "shop-production", NodeID: "n1"},
		{Space: "acme-production", NodeID: "n1"},
	}

	got := MergeDeclared(nodes, declared, nodeIDs())
	if len(got[0].Spaces) != 2 {
		t.Fatalf("spaces = %d, want 2 (the stopped one must stay listed)", len(got[0].Spaces))
	}
	var acme *Space
	for i := range got[0].Spaces {
		if got[0].Spaces[i].Name == "acme-production" {
			acme = &got[0].Spaces[i]
		}
	}
	if acme == nil {
		t.Fatal("a declared space absent from the node must still be listed")
	}
	if acme.Origin != OriginDeclared || acme.Status != "stopped" {
		t.Errorf("origin/status = %q/%q, want declared/stopped", acme.Origin, acme.Status)
	}
}

// The mirror image: fresh, production and sklp-dev-runnerbuild-test showed up
// as real spaces though no repo declares them.
func TestMergeDeclared_UndeclaredSpaceIsOrphan(t *testing.T) {
	nodes := []Node{{Name: "ci-instance-v1-server", Spaces: []Space{
		{Name: "shop-production", Status: "running"},
		{Name: "sklp-dev-runnerbuild-test", Status: "unknown"},
	}}}
	declared := []DeclaredSpace{{Space: "shop-production", NodeID: "n1"}}

	got := MergeDeclared(nodes, declared, nodeIDs())
	byName := map[string]Space{}
	for _, s := range got[0].Spaces {
		byName[s.Name] = s
	}
	if byName["shop-production"].Origin != OriginRunning {
		t.Errorf("declared+running = %q, want running", byName["shop-production"].Origin)
	}
	if byName["sklp-dev-runnerbuild-test"].Origin != OriginOrphan {
		t.Errorf("undeclared leftover = %q, want orphan", byName["sklp-dev-runnerbuild-test"].Origin)
	}
}

// A node that could not be read reports nothing; claiming its declared spaces
// are stopped would invent a state nobody checked.
func TestMergeDeclared_UnreachableNodeGetsNoStoppedRows(t *testing.T) {
	nodes := []Node{{Name: "ci-instance-v1-server", Err: "dial tcp: timeout"}}
	declared := []DeclaredSpace{{Space: "acme-production", NodeID: "n1"}}

	got := MergeDeclared(nodes, declared, nodeIDs())
	if len(got[0].Spaces) != 0 {
		t.Fatalf("spaces = %v, want none on an unreachable node", got[0].Spaces)
	}
}

// Declarations are per node: another node's spaces must not leak in.
func TestMergeDeclared_ScopedToItsOwnNode(t *testing.T) {
	nodes := []Node{{Name: "ci-instance-v1-server", Spaces: nil}}
	declared := []DeclaredSpace{{Space: "elsewhere-production", NodeID: "n2"}}

	got := MergeDeclared(nodes, declared, nodeIDs())
	if len(got[0].Spaces) != 0 {
		t.Fatalf("spaces = %v, want none (declared on another node)", got[0].Spaces)
	}
}

// No declarations at all leaves the node inventory exactly as it was.
func TestMergeDeclared_NoDeclarationsKeepsInventory(t *testing.T) {
	nodes := []Node{{Name: "ci-instance-v1-server", Spaces: []Space{{Name: "x", Status: "running"}}}}

	got := MergeDeclared(nodes, nil, nodeIDs())
	if len(got[0].Spaces) != 1 || got[0].Spaces[0].Origin != OriginOrphan {
		t.Fatalf("spaces = %v, want the one space marked orphan", got[0].Spaces)
	}
}

// The sync state is the question the node alone cannot answer: is what runs
// what Git asked for. It must reach the running row, not only the stopped one.
func TestMergeDeclared_CarriesSyncOntoARunningSpace(t *testing.T) {
	nodes := []Node{{Name: "ci-instance-v1-server", Spaces: []Space{{Name: "shop-production", Status: "running"}}}}
	declared := []DeclaredSpace{{
		Space: "shop-production", NodeID: "n1",
		Sync: SyncFailed, LastError: "apply refused: unknown service",
	}}

	got := MergeDeclared(nodes, declared, nodeIDs())
	s := got[0].Spaces[0]
	if s.Sync != SyncFailed {
		t.Errorf("sync = %q, want failed", s.Sync)
	}
	if s.LastError == "" {
		t.Error("the reason a deployment failed must reach the UI")
	}
}

// A declared space that was never applied reads pending, not failed.
func TestMergeDeclared_CarriesSyncOntoAStoppedSpace(t *testing.T) {
	nodes := []Node{{Name: "ci-instance-v1-server"}}
	declared := []DeclaredSpace{{Space: "acme-production", NodeID: "n1", Sync: SyncPending}}

	got := MergeDeclared(nodes, declared, nodeIDs())
	if len(got[0].Spaces) != 1 {
		t.Fatalf("spaces = %d, want 1", len(got[0].Spaces))
	}
	if got[0].Spaces[0].Sync != SyncPending {
		t.Errorf("sync = %q, want pending", got[0].Spaces[0].Sync)
	}
}

// "Is the space healthy" and "did my push land" are different questions: a
// space can be healthy on services reconciled days ago, so the time of the
// last apply has to reach the UI on its own.
func TestMergeDeclared_CarriesLastAppliedAt(t *testing.T) {
	at := time.Date(2026, 9, 4, 2, 31, 14, 0, time.UTC)
	nodes := []Node{{Name: "ci-instance-v1-server", Spaces: []Space{{Name: "shop-production", Status: "running"}}}}
	declared := []DeclaredSpace{
		{Space: "shop-production", NodeID: "n1", Sync: SyncSynced, LastAppliedAt: &at},
		{Space: "shop-staging", NodeID: "n1", Sync: SyncSynced, LastAppliedAt: &at},
	}

	got := MergeDeclared(nodes, declared, nodeIDs())
	for _, sp := range got[0].Spaces {
		if sp.LastAppliedAt == nil {
			t.Fatalf("%s: last applied time missing", sp.Name)
		}
		if !sp.LastAppliedAt.Equal(at) {
			t.Errorf("%s: last applied = %v, want %v", sp.Name, sp.LastAppliedAt, at)
		}
	}
}
