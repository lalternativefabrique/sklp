// Package domain holds the read model of the spaces bounded context.
//
// A Node is a machine (a VM or a pod) running sklet, and a Space is one
// execution unit on it. This context owns no lifecycle: a node is the source of
// truth for the STATE of what it runs, and skalpai only reads it — a stored
// inventory of running containers would be a second truth that goes stale the
// moment one dies.
//
// EXISTENCE is a different question, and Git answers it: a space exists because
// a deploy.yaml declares it. Listing only what a node runs made a stopped space
// disappear with no way back to it, while leftover state on disk that no repo
// declares was listed as if it were real. MergeDeclared reconciles the two, the
// way image watches are reconciled from their own file.
//
// The node list itself is borrowed from the gateway context, which already
// owns a stack's registered nodes (address, bearer token, cert fingerprint) to
// push routes to them. This context adds no registry of its own.
package domain

import (
	"context"
	"time"
)

// Node is one machine and what it answered when asked for its inventory.
// Spaces and Err are mutually exclusive: a node that could not be read is still
// reported, carrying why, because a machine missing from the list reads as one
// running nothing.
type Node struct {
	Name string `json:"name"`
	// Hostname is what the machine calls itself, kept beside Name rather than
	// replacing it: an operator recognises the hostname, but only Name resolves
	// a node anywhere else.
	Hostname string  `json:"hostname,omitempty"`
	Target   string  `json:"target"`
	Spaces   []Space `json:"spaces"`
	Err      string  `json:"error,omitempty"`
}

// Space is one execution unit on a node, as its runc state reports it.
type Space struct {
	Name       string      `json:"name"`
	Status     string      `json:"status"`
	Supervised bool        `json:"supervised"`
	Containers []Container `json:"containers"`
	Volumes    []string    `json:"volumes,omitempty"`
	Repo       string      `json:"repo,omitempty"`
	Origin     Origin      `json:"origin,omitempty"`
	Sync       SyncStatus  `json:"sync,omitempty"`
	LastError  string      `json:"last_error,omitempty"`
	// LastAppliedAt is when Skalpai last reconciled this space's manifest onto
	// the node, nil when it never did.
	LastAppliedAt *time.Time `json:"last_applied_at,omitempty"`
}

// Container is one runc container of a space.
type Container struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// Reader enumerates a stack's registered nodes and their spaces. The
// infrastructure layer implements it against the nodes' control API.
type Reader interface {
	ListNodes(ctx context.Context, stackID string) []Node
}

// ServiceState is one service's aggregate state plus its live instances, as
// reported by the supervisor that runs a space.
type ServiceState struct {
	Name  string `json:"name"`
	State string `json:"state"`
	// Image is the OCI reference the desired manifest names for this service.
	// The node has always served it on /v1/state; it is read here so the graph
	// can label a service with what it actually runs.
	Image string `json:"image,omitempty"`
	// Oneshot marks a job: expected to exit, and the only kind of service that
	// can be re-run. A completed job and a stopped service look alike otherwise.
	Oneshot   bool            `json:"oneshot,omitempty"`
	Instances []InstanceState `json:"instances"`
	Rollouts  []RolloutState  `json:"rollouts,omitempty"`
	Runs      []JobRunState   `json:"runs,omitempty"`
	// RejectedReason is set when the node refuses to replay this service's
	// spec after rolling it back, which freezes the service on its previous
	// definition and makes every apply of that spec a silent no-op.
	RejectedReason string `json:"rejected_reason,omitempty"`
}

// JobRunState is one execution of a job as the node recorded it, oldest last.
type JobRunState struct {
	ID         string   `json:"id"`
	Instance   string   `json:"instance"`
	Image      string   `json:"image,omitempty"`
	Trigger    string   `json:"trigger,omitempty"`
	ExitCode   int      `json:"exit_code"`
	Failed     bool     `json:"failed,omitempty"`
	LogTail    []string `json:"log_tail,omitempty"`
	StartedAt  string   `json:"started_at"`
	FinishedAt string   `json:"finished_at"`
}

// RolloutState is one rolling update of a service as the node recorded it,
// the in-flight one last.
type RolloutState struct {
	ID         string   `json:"id"`
	FromImage  string   `json:"from_image"`
	ToImage    string   `json:"to_image"`
	Result     string   `json:"result"`
	Reason     string   `json:"reason,omitempty"`
	ExitCode   int      `json:"exit_code,omitempty"`
	LogTail    []string `json:"log_tail,omitempty"`
	StartedAt  string   `json:"started_at"`
	FinishedAt string   `json:"finished_at,omitempty"`
}

// InstanceState is one running (or scheduled-to-restart) replica.
type InstanceState struct {
	ID    string `json:"id"`
	State string `json:"state"`
	PID   int    `json:"pid"`
	IP    string `json:"ip,omitempty"`
	Port  int    `json:"port,omitempty"`

	// Restarts and StartedAt are what separate a flapping instance from a
	// steady one: this state is a snapshot, so an instance that dies and
	// returns between two reads reports the same "ready" as one that never
	// moved.
	Restarts  int    `json:"restarts,omitempty"`
	StartedAt string `json:"started_at,omitempty"`

	// RestartAt is when a dead instance is due back, BackoffMS the delay the
	// next restart waits — it climbs 1s→30s while a service keeps dying.
	RestartAt string `json:"restart_at,omitempty"`
	BackoffMS int64  `json:"backoff_ms,omitempty"`

	// JobFails counts a oneshot's non-zero exits, Held marks one that spent
	// its retry cap: terminal, and it holds the whole space short of ready.
	JobFails int  `json:"job_fails,omitempty"`
	Held     bool `json:"held,omitempty"`
}

// SpaceState is the supervisor's live view of one space: not the runc
// process status Reader already reports, but the readiness state machine
// (pending/starting/ready/failed/restarting) driving it.
type SpaceState struct {
	Space    string         `json:"space"`
	AllReady bool           `json:"all_ready"`
	Services []ServiceState `json:"services"`
	Err      string         `json:"error,omitempty"`
}

// StateReader reads a single space's live supervisor state. Unlike Reader it
// is scoped to one node.
//
// nodeID names the node; empty is only enough for a stack with a single one.
type StateReader interface {
	ReadState(ctx context.Context, stackID, spaceName, nodeID string) SpaceState
}

// CommandOutcome classifies how a node answered an action, so the API can
// tell "there is nothing to act on" from "the node could not be reached"
// from "the node said no" — three situations the UI presents differently.
type CommandOutcome int

const (
	CommandOK CommandOutcome = iota
	// CommandNotFound: no node registered for the space, or the node knows no
	// such service.
	CommandNotFound
	// CommandUnreachable: the node did not answer at all.
	CommandUnreachable
	// CommandRefused: the node answered, and declined.
	CommandRefused
	// CommandConflict: the action cannot apply to the current state (a job whose
	// run is already in flight), as opposed to a node that said no outright.
	CommandConflict
)

// CommandResult is a node's answer to an action.
type CommandResult struct {
	Outcome CommandOutcome
	Detail  string
}

// Commander drives a space through its node's control API. Same node
// resolution as StateReader: nodeID names the record directly, and is the
// reliable path on a multi-space node.
type Commander interface {
	RestartService(ctx context.Context, stackID, spaceName, nodeID, service string) CommandResult
	RunJob(ctx context.Context, stackID, spaceName, nodeID, job string) CommandResult
	StopSpace(ctx context.Context, stackID, spaceName, nodeID string) CommandResult
	StartSpace(ctx context.Context, stackID, spaceName, nodeID string) CommandResult
	DeleteSpace(ctx context.Context, stackID, spaceName, nodeID string) CommandResult
}
