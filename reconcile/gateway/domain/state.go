package domain

import (
	"context"
	"time"
)

// ServiceState mirrors sklp's control.ServiceState: one service's aggregate
// state plus its live instances.
type ServiceState struct {
	Name  string
	State string

	// Image is the OCI reference the node's own manifest names for this
	// service. Empty from a node too old to report it.
	Image string

	// Oneshot marks a job: expected to exit, and the only kind of service that
	// can be re-run. False from a node too old to report it.
	Oneshot   bool
	Instances []InstanceState
	// Rollouts is the service's recent rolling updates and how each ended,
	// the in-flight one last. Empty from a node too old to report them.
	Rollouts []RolloutState
	// Runs is a job's recent executions, oldest last. It is what tells a run
	// that just happened from the one before it: the terminal state carries no
	// clock, and the container log holds only the last run. Empty from a node
	// too old to report them.
	Runs []JobRunState

	// RejectedReason is set when the node refuses to replay this service's
	// spec because its rollout was rolled back: the service is frozen on its
	// previous definition and every apply of that spec is a no-op. Empty from
	// a node too old to report it.
	RejectedReason string
}

// JobRunState mirrors sklp's control.JobRunState: one execution of a job.
type JobRunState struct {
	ID         string
	Instance   string
	Image      string
	Trigger    string
	ExitCode   int
	Failed     bool
	LogTail    []string
	StartedAt  string
	FinishedAt string
}

// RolloutState mirrors sklp's control.RolloutState: one rolling update of a
// service.
type RolloutState struct {
	ID         string
	FromImage  string
	ToImage    string
	Result     string
	Reason     string
	ExitCode   int
	LogTail    []string
	StartedAt  string
	FinishedAt string
}

// InstanceState mirrors sklp's control.InstanceState: one running (or
// scheduled-to-restart) replica.
type InstanceState struct {
	ID    string
	State string
	PID   int

	// IP and Port are the instance's address on the space network, as the
	// supervisor observed it. Empty from a node too old to report them.
	IP   string
	Port int

	// Restarts is how many times the instance came back after its first run,
	// StartedAt when the current run began (RFC3339). A state read is a
	// snapshot, so an instance that dies and returns between two reads looks
	// steadily ready; these two are what tell that flap apart. Zero from a
	// node too old to report them.
	Restarts  int
	StartedAt string

	// RestartAt is when a dead instance is due back (RFC3339), BackoffMS the
	// delay the next restart would wait — it climbs 1s→30s while a service
	// keeps dying and resets after a stable run. Zero from a node too old to
	// report them.
	RestartAt string
	BackoffMS int64

	// JobFails counts a oneshot's non-zero exits, Held marks one that spent
	// its retry cap and will never be relaunched. Zero from a node too old to
	// report them.
	JobFails int
	Held     bool
}

// NodeState is a node's whole-space view, as sklp's GET /v1/state reports it.
type NodeState struct {
	Space    string
	AllReady bool
	Services []ServiceState
}

// StateFetcher reads a space's live deploy state. It is the domain's view of
// the node control API's GET /v1/spaces/<space>/state (or the unprefixed
// GET /v1/state on a legacy single-space node — see the infrastructure
// implementation), kept separate from Pusher: a state read shares the node's
// transport (bearer token + pinned cert) but not its write path.
type StateFetcher interface {
	State(ctx context.Context, node *Node, space string) (NodeState, error)
}

// CertFetcher reads the certificates a node's front door serves. It is its own
// interface rather than a method on StateFetcher: the certs read is node-scoped
// and dials every routed host, so it answers on a different timescale than a
// space's state read.
type CertFetcher interface {
	Certs(ctx context.Context, node *Node) ([]HostCert, error)
}

// GatewayReloader re-ensures a node's front door. Separate from Pusher: this
// carries no desired state, it only asks the node to converge on what it was
// already told — which is what re-attempts a certificate that never issued.
type GatewayReloader interface {
	ReloadGateway(ctx context.Context, node *Node) error
}

// HostCert is the TLS certificate one routed host is served, as the node read
// it off its own front door. Err is set when the handshake failed, and the
// other fields are then zero: a host that does not answer is a different state
// from one served a bad certificate.
type HostCert struct {
	Host       string
	Issuer     string
	SelfSigned bool
	NotAfter   time.Time
	Err        string
}
