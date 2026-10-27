package domain

import (
	"context"
	"time"
)

// Origin says whether a space is declared in Git, running on a node, or both.
//
// A space exists because a deploy.yaml declares it, not because a container
// happens to be up: reading only the node made a stopped space vanish from the
// list with no way back to it, while leftovers on disk that no repo declares
// showed up as if they were real.
type Origin string

const (
	// OriginDeclared: declared in Git, absent from the node — stopped, or not
	// applied yet. Startable.
	OriginDeclared Origin = "declared"
	// OriginRunning: declared in Git and present on the node.
	OriginRunning Origin = "running"
	// OriginOrphan: present on the node, declared nowhere. Leftover state.
	OriginOrphan Origin = "orphan"
)

// SyncStatus is how a declared space's applied state compares to what Git
// says — the question "is what runs what we asked for", which the node alone
// cannot answer and the repo alone cannot either.
type SyncStatus string

const (
	// SyncSynced: the manifest Skalpai last applied matches the declaration,
	// and the node reports it healthy.
	SyncSynced SyncStatus = "synced"
	// SyncDeploying: applied, not converged yet — the image is still pulling
	// or a readiness probe has not passed. Not a failure.
	SyncDeploying SyncStatus = "deploying"
	// SyncFailed: the last apply (or its health check) reported an error.
	SyncFailed SyncStatus = "failed"
	// SyncPending: declared, never applied — a space Skalpai knows about but
	// has not reconciled yet.
	SyncPending SyncStatus = "pending"
)

// DeclaredSpace is one space a repo's deploy.yaml declares on a node, with
// the state of the last reconciliation Skalpai performed for it.
type DeclaredSpace struct {
	Space  string
	NodeID string
	Repo   string

	Sync SyncStatus
	// LastAppliedAt is when Skalpai last pushed this manifest to the node,
	// nil when it never did.
	LastAppliedAt *time.Time
	// LastError is why the last apply failed, empty when it did not.
	LastError string
}

// DeclarationReader lists what a stack's repos declare, independently of what
// any node currently runs.
type DeclarationReader interface {
	ListDeclared(ctx context.Context, stackID string) []DeclaredSpace
}

// MergeDeclared folds the declared spaces into the nodes' live inventory:
// declared-and-absent spaces are appended as OriginDeclared, and everything
// already reported by a node is labelled running or orphan.
//
// A nil reader leaves the inventory untouched, so a caller without the
// deployconfig context keeps the node-only behaviour.
func MergeDeclared(nodes []Node, declared []DeclaredSpace, nodeIDByName map[string]string) []Node {
	byNode := map[string][]DeclaredSpace{}
	for _, d := range declared {
		byNode[d.NodeID] = append(byNode[d.NodeID], d)
	}

	for i := range nodes {
		id := nodeIDByName[nodes[i].Name]
		decl := byNode[id]
		declBySpace := map[string]DeclaredSpace{}
		for _, d := range decl {
			declBySpace[d.Space] = d
		}

		seen := map[string]bool{}
		for j := range nodes[i].Spaces {
			name := nodes[i].Spaces[j].Name
			seen[name] = true
			if d, ok := declBySpace[name]; ok {
				nodes[i].Spaces[j].Origin = OriginRunning
				nodes[i].Spaces[j].Sync = d.Sync
				nodes[i].Spaces[j].LastError = d.LastError
				nodes[i].Spaces[j].LastAppliedAt = d.LastAppliedAt
				if nodes[i].Spaces[j].Repo == "" {
					nodes[i].Spaces[j].Repo = d.Repo
				}
				continue
			}
			nodes[i].Spaces[j].Origin = OriginOrphan
		}

		// A node that could not be read reports no spaces; appending declared
		// ones there would claim they are stopped when nothing was checked.
		if nodes[i].Err != "" {
			continue
		}
		for _, d := range decl {
			if seen[d.Space] {
				continue
			}
			nodes[i].Spaces = append(nodes[i].Spaces, Space{
				Name:          d.Space,
				Status:        "stopped",
				Origin:        OriginDeclared,
				Repo:          d.Repo,
				Sync:          d.Sync,
				LastError:     d.LastError,
				LastAppliedAt: d.LastAppliedAt,
			})
		}
	}
	return nodes
}

// NodeIDResolver maps a node's display name to its registered id, so declared
// spaces (keyed by node id) can be matched to the inventory (keyed by name).
type NodeIDResolver interface {
	NodeIDsByName(ctx context.Context, stackID string) map[string]string
}
