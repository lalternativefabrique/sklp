package domain

import (
	"context"
	"path"
)

// NodeTarget is the transport a Manifest applies against: a node's address
// plus its already-decrypted bearer token and pinned cert fingerprint. Kept
// as its own small struct (rather than importing gatewaydomain.Node here)
// so this package's only dependency on gateway is the one
// reconcile_from_git already has — gatewaydomain.NodeRepository, to resolve
// Manifest.NodeID — matching the existing precedent instead of adding a
// second one.
type NodeTarget struct {
	ServerURL     string
	Space         string
	Token         string
	CAFingerprint string
	Stack         string
}

// StackOf names the stack a repo-relative manifest path belongs to
// (".sklp/prod/deploy.yaml" -> "prod"), the ownership tag `sklp deploy` stamps.
func StackOf(manifestPath string) string {
	dir := path.Base(path.Dir(manifestPath))
	if dir == ".sklp" || dir == "." || dir == "/" {
		return ""
	}
	return dir
}

// ApplyResult is Skalpai's own mirror of the node's POST /v1/apply response
// (spacemanager/control.ApplyResult) — kept separate rather than imported so
// this package never needs the embedded_runtime build tag the sklp binaries carry
// (same reasoning as gatewaydomain.NodeState mirroring control.ServiceState;
// see the server's gateway domain).
type ApplyResult struct {
	Result   string
	Changes  []string
	Warnings []string
}

// Applier POSTs a Manifest's resolved service set to a node's control API —
// the domain's view of POST /v1/spaces/<space>/apply (or /v1/apply on a
// legacy single-space node). One call handles both a first-ever deploy
// (space does not exist yet) and a re-apply (drift detected): the node's
// handler is create-or-update, idempotent — see issue 206ec5bd's transport
// research, spacemanager/control/apply_linux.go's handleApply.
type Applier interface {
	Apply(ctx context.Context, target NodeTarget, m *Manifest) (ApplyResult, error)
}
