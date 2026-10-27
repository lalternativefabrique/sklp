// Package domain holds the aggregate root of the deploy-config bounded
// context.
//
// A Deployment binds one deploy.yaml to the node/space deploy-root.yaml
// declares for it, plus a fingerprint of the manifest last applied from Git — the piece
// imageupdate's Watch deliberately does not need (see
// the server's imageupdate Watch: a Watch names a
// service and a tag, never a manifest). This aggregate closes that gap for
// the topology itself: Skalpai can apply a deploy.yaml's full service set to
// a node, not just roll an existing service onto a new tag.
package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrValidation is the parent of every rejection NewDeployment can produce.
var ErrValidation = errors.New("invalid deployment")

var (
	ErrEmptyName   = fmt.Errorf("%w: name is required", ErrValidation)
	ErrEmptyTarget = fmt.Errorf("%w: a node (or a legacy node_id) is required", ErrValidation)
	ErrNotFound    = errors.New("deployment not found")
)

// Deployment binds one deploy.yaml (identified by its repo-relative path) to
// the node/space it targets, and records the last manifest Skalpai applied
// from Git — so a later push can tell "nothing changed" from "re-apply".
type Deployment struct {
	ID      string
	StackID string
	Name    string // stable identifier: the deploy.yaml's repo-relative path
	// Node is the registered name of the host this deployment targets, as
	// written in the deploy-root. It survives the node changing address.
	Node string
	// NodeID is the superseded identity, an opaque id only the node registry
	// could resolve. Empty on anything declared with a server.
	NodeID string
	Space  string
	// ProjectID is the project whose repo declares this deployment, so a
	// space's node logs can sit next to that project's own telemetry.
	ProjectID string
	// LastAppliedHash is Manifest.Hash() of the last successful apply: the
	// file as committed, plus the resolved ${VAR} values.
	LastAppliedHash string
	LastAppliedAt   *time.Time
	// Ready reflects the POST-APPLY health check (GET
	// /v1/spaces/<space>/state's AllReady) — distinct from whether the apply
	// call itself succeeded. An apply can return 200 (the node accepted and
	// is reconciling toward the manifest) while the service has not
	// converged yet (image still pulling, readiness probe still failing);
	// Ready=false with LastError="" is exactly that state, told apart from
	// a hard apply failure (LastError set — see RecordFailure) so the UI can
	// show "deploying" rather than "broken". See issue 206ec5bd's step 2.
	Ready         bool
	LastCheckedAt *time.Time
	LastError     string
	// ErrorFromCheck marks LastError as a health-check transport failure, the
	// only kind a later ready answer from the node disproves.
	ErrorFromCheck bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// NewDeployment validates and builds a Deployment. Space defaults to
// "production", matching gateway.Node's own default.
func NewDeployment(stackID, name, nodeName, nodeID, space string) (*Deployment, error) {
	if strings.TrimSpace(stackID) == "" {
		return nil, fmt.Errorf("%w: stack_id is required", ErrValidation)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, ErrEmptyName
	}
	nodeName = strings.TrimSpace(nodeName)
	nodeID = strings.TrimSpace(nodeID)
	if nodeName == "" && nodeID == "" {
		return nil, ErrEmptyTarget
	}
	space = strings.TrimSpace(space)
	if space == "" {
		space = "production"
	}
	now := time.Now().UTC()
	return &Deployment{
		ID:        uuid.New().String(),
		StackID:   stackID,
		Name:      name,
		Node:      nodeName,
		NodeID:    nodeID,
		Space:     space,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// RecordApplied marks a successful apply and remembers its hash, so the next
// reconcile can skip an unchanged manifest instead of re-POSTing it. Ready
// resets to false: a freshly applied manifest has not been health-checked
// yet — the caller calls RecordHealth once the post-apply check runs (see
// reconcile_deploy/handler.go).
func (d *Deployment) RecordApplied(hash string, at time.Time) {
	d.LastAppliedHash = hash
	utc := at.UTC()
	d.LastAppliedAt = &utc
	d.Ready = false
	d.LastError = ""
	d.ErrorFromCheck = false
	d.UpdatedAt = time.Now().UTC()
}

// RecordFailure stores why the last apply did not converge, so the UI can
// show a deployment that is configured but failing rather than silently
// stale — mirrors gateway.Node.RecordFailure. Ready resets to false: an
// apply that failed outright cannot have made the service ready.
func (d *Deployment) RecordFailure(msg string) {
	d.Ready = false
	d.LastError = msg
	d.ErrorFromCheck = false
	d.UpdatedAt = time.Now().UTC()
}

// RecordHealth stores the outcome of the POST-APPLY health check (GET
// /v1/spaces/<space>/state's AllReady). Unlike RecordFailure, this never
// touches LastAppliedHash/LastAppliedAt: the apply itself already
// succeeded (RecordApplied ran first) — a health check finding the service
// not yet ready is not grounds to forget that the manifest WAS applied, so
// the next reconcile still sees LastAppliedHash match an unchanged
// deploy.yaml and correctly skips re-POSTing it. checkErr, when non-empty,
// is a TRANSPORT failure reaching /v1/spaces/<space>/state (node
// unreachable, TLS error) — distinct from ready=false (the node answered;
// the service just is not converged yet).
func (d *Deployment) RecordHealth(ready bool, checkErr string, at time.Time) {
	d.Ready = ready
	d.TouchChecked(at)
	if checkErr != "" {
		d.LastError = checkErr
		d.ErrorFromCheck = true
	}
	d.UpdatedAt = time.Now().UTC()
}

// ClearCheckFailure drops a failure the node has since stopped reproducing,
// so a space that recovered stops reading as failed without waiting for a
// push. Only the health check calls it, and only on a node that answered
// every service ready.
//
// It is deliberately NOT folded into RecordHealth: a pre-apply failure — an
// unresolvable ${VAR}, a token that will not open — is a fault on the Skalpai
// side, and the node running the PREVIOUS manifest happily answers ready
// while it stands. Clearing on readiness alone would erase exactly the errors
// no node can speak to.
func (d *Deployment) ClearCheckFailure() {
	if !d.ErrorFromCheck {
		return
	}
	d.LastError = ""
	d.ErrorFromCheck = false
	d.UpdatedAt = time.Now().UTC()
}

// TouchChecked records that the node was polled, whatever it answered. The
// health check writes it on every poll, not only when something changed, so
// "least recently checked" stays an ordering a scheduler can trust rather
// than a timestamp frozen at the last state change.
func (d *Deployment) TouchChecked(at time.Time) {
	utc := at.UTC()
	d.LastCheckedAt = &utc
}
