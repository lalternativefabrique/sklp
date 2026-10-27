package domain

import (
	"errors"
	"strings"
	"testing"
)

const validRoot = `
kind: deploy-root
node_id: node-1
deployments:
  - space: tornade-production
    path: .sklp/prod/deploy.yaml
  - space: piper-production
    path: .sklp/prod/piper.yaml
`

func TestParseRootReadsEveryDeployment(t *testing.T) {
	r, err := ParseRoot([]byte(validRoot))
	if err != nil {
		t.Fatalf("ParseRoot: %v", err)
	}
	if len(r.Deployments) != 2 {
		t.Fatalf("deployments = %d, want 2", len(r.Deployments))
	}
	// The root's node_id is the default, so the common case names it once.
	for _, d := range r.Deployments {
		if d.NodeID != "node-1" {
			t.Errorf("%s: node_id = %q, want the root default", d.Space, d.NodeID)
		}
	}
}

func TestParseRootEntryOverridesNodeID(t *testing.T) {
	r, err := ParseRoot([]byte(`
kind: deploy-root
node_id: node-1
deployments:
  - space: a
    path: p
  - space: b
    path: p2
    node_id: node-2
`))
	if err != nil {
		t.Fatalf("ParseRoot: %v", err)
	}
	if r.Deployments[1].NodeID != "node-2" {
		t.Fatalf("node_id = %q, want the entry override", r.Deployments[1].NodeID)
	}
}

// A file at RootPath that is not a root (a mis-scoped fetch returning the
// deploy.yaml) must fall back to the single manifest, not reconcile nothing.
func TestParseRootRejectsAForeignFileDistinctly(t *testing.T) {
	_, err := ParseRoot([]byte("version: 1\nkind: deploy\nname: prod\n"))
	if !errors.Is(err, ErrNotARoot) {
		t.Fatalf("err = %v, want ErrNotARoot so the caller can fall back", err)
	}
}

// A typo that silently dropped a production space would be worse than a
// refusal, so an incomplete entry fails the whole file.
func TestParseRootRejectsIncompleteEntries(t *testing.T) {
	for name, raw := range map[string]string{
		"no space":   "kind: deploy-root\nnode_id: n\ndeployments:\n  - path: p\n",
		"no path":    "kind: deploy-root\nnode_id: n\ndeployments:\n  - space: s\n",
		"no node_id": "kind: deploy-root\ndeployments:\n  - space: s\n    path: p\n",
		"empty":      "kind: deploy-root\ndeployments: []\n",
	} {
		if _, err := ParseRoot([]byte(raw)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// Two entries for one space would race each other on every push.
func TestParseRootRejectsADuplicateSpace(t *testing.T) {
	_, err := ParseRoot([]byte(`
kind: deploy-root
node_id: n
deployments:
  - space: dup
    path: a
  - space: dup
    path: b
`))
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("err = %v, want a duplicate-space rejection", err)
	}
}

// A node is named by the address it answers on, so a reconcile reads its
// destination straight off the file instead of resolving an id against the
// registry first.
func TestParseRootTakesNodeNameAsIdentity(t *testing.T) {
	r, err := ParseRoot([]byte(`
kind: deploy-root
node: node-a
deployments:
  - space: altfab-production
    path: .sklp/prod/deploy.yaml
  - space: www-production
    path: .sklp/prod/www.yaml
    node: other-node
`))
	if err != nil {
		t.Fatalf("ParseRoot: %v", err)
	}
	if got := r.Deployments[0].Node; got != "node-a" {
		t.Errorf("entry 0 server = %q, want the root's default", got)
	}
	if got := r.Deployments[1].Node; got != "other-node" {
		t.Errorf("entry 1 server = %q, want its own override", got)
	}
}

// An entry naming a server must not also inherit the root's node_id: carrying
// both would leave the reconcile free to resolve the one the entry did not ask
// for, which is how a deployment silently lands on the wrong node.
func TestParseRootNamedEntryDoesNotInheritNodeID(t *testing.T) {
	r, err := ParseRoot([]byte(`
kind: deploy-root
node_id: node-1
deployments:
  - space: a
    path: p
    node: node-a
`))
	if err != nil {
		t.Fatalf("ParseRoot: %v", err)
	}
	if r.Deployments[0].NodeID != "" {
		t.Errorf("node_id = %q, want it dropped once a node is named", r.Deployments[0].NodeID)
	}
}

// The roots already deployed name a node_id and must keep reconciling.
func TestParseRootStillAcceptsNodeID(t *testing.T) {
	r, err := ParseRoot([]byte(`
kind: deploy-root
node_id: node-1
deployments:
  - space: a
    path: p
`))
	if err != nil {
		t.Fatalf("ParseRoot: %v", err)
	}
	if r.Deployments[0].NodeID != "node-1" || r.Deployments[0].Node != "" {
		t.Errorf("got node=%q node_id=%q, want the legacy id alone",
			r.Deployments[0].Node, r.Deployments[0].NodeID)
	}
}

// Naming neither is the one case that cannot be reconciled at all.
func TestParseRootRejectsEntryWithNoTarget(t *testing.T) {
	_, err := ParseRoot([]byte(`
kind: deploy-root
deployments:
  - space: a
    path: p
`))
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
}
