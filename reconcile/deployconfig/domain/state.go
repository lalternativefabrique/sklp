package domain

import "context"

// ServiceState mirrors sklp's control.ServiceState — same shape as
// gatewaydomain.ServiceState (the server's gateway domain), duplicated
// rather than imported: this context does not depend on gateway/domain
// beyond NodeRepository (see applier.go's doc), and gateway's own
// StateFetcher hard-codes the unprefixed /v1/state route, which only ever
// answers for the space a node's supervisor happened to start on — wrong
// for a multi-space node's target.space (see infrastructure/
// control_state_fetcher.go's doc). A shared type would not have made the
// route correct.
type ServiceState struct {
	Name      string
	State     string
	Instances []InstanceState
}

// InstanceState mirrors sklp's control.InstanceState.
type InstanceState struct {
	ID    string
	State string
	PID   int
}

// NodeState is one space's live view, as GET /v1/spaces/<space>/state
// reports it.
type NodeState struct {
	Space    string
	AllReady bool
	Services []ServiceState
}

// StateFetcher reads a space's live deploy state — the domain's view of
// GET /v1/spaces/<space>/state, used by reconcile_deploy to confirm an
// Apply actually converged (see issue 206ec5bd's step 2) rather than
// trusting the apply call's own 200 response alone.
type StateFetcher interface {
	State(ctx context.Context, target NodeTarget) (NodeState, error)
}
