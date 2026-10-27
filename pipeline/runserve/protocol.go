// Package runserve is the node-side HTTP API that executes pipelines on
// request. It is the counterpart of spacemanager/control for CI: control
// serves the DEPLOY plane (apply/state/logs of a running space), runserve
// serves the RUN plane (execute a pipeline, stream its output, return the
// report).
//
// They stay separate on purpose. control is scoped to a space and reads a
// live supervisor; a CI node has neither. What they DO share is the node
// identity — the same self-signed cert and the same host-wide Bearer token
// from control — so one node exposes one credential regardless of which
// plane a caller reaches.
//
// The wire format is deliberately chunk-per-line rather than a single JSON
// response: the caller (core, streaming to SSE; the CLI, printing to a
// terminal) needs the pipeline's stderr AS IT HAPPENS, not once the run is
// over. The report arrives last, on its own frame.
package runserve

const (
	APIVersion = "v1"

	// RouteHealth answers liveness with the node's sklp version, so a caller
	// can tell a reachable node from a stale DNS entry before dispatching.
	RouteHealth = "/v1/health"

	// RouteRun executes one pipeline and streams the result:
	//   POST /v1/run   body: RunRequest   response: NDJSON of Frame
	RouteRun = "/v1/run"

	// RouteRunStatus reads back a run's state independently of the stream:
	//   GET /v1/run/{run_id}   response: RunStatusResponse
	// It exists for a caller that lost the /v1/run connection (a redeploy,
	// a network blip) — the node keeps executing regardless (see
	// handleRun's use of context.WithoutCancel), this is how the caller
	// finds out what happened without re-triggering the run.
	RouteRunStatus = "/v1/run/"

	// RouteRunCancel stops a run that is still executing:
	//   DELETE /v1/run/{run_id}   response: RunCancelResponse
	// It shares RouteRunStatus's path, dispatched by method. Cancelling is
	// the ONLY way to stop work on a node: a run is deliberately detached
	// from its caller's connection (see handleRun), so hanging up leaves it
	// running, and marking it aborted in a caller's own database stops
	// nothing here.

	// RouteCacheClean runs the node's own housekeeping (the same logic behind
	// `sklp clean`) in-process and returns once it finishes:
	//   POST /v1/cache/clean   body: CacheCleanRequest   response: CacheCleanResponse
	// A CI node accumulates rootfs/image caches that only a rebuild or a
	// clean recreates; this exists so core (and the UI button behind it) can
	// trigger that without an operator SSHing in.
	RouteCacheClean = "/v1/cache/clean"
)

// RunCancelResponse is the DELETE /v1/run/{id} reply. Cancelled is false when
// the run had already finished — the request succeeded, but stopped nothing.
type RunCancelResponse struct {
	Cancelled bool   `json:"cancelled"`
	Status    string `json:"status"`
}

// RunRequest is the POST /v1/run body: what to clone and which pipeline to
// execute. The node always self-clones — it holds no repository of its own —
// so RepoURL is required and carries its own credentials when the repo is
// private (https://x-access-token:<PAT>@github.com/owner/repo.git).
type RunRequest struct {
	// RunID identifies this run for RouteRunStatus. Required: without it the
	// node has no key to remember the run under, and a caller that loses the
	// stream has no way to ask about it again.
	RunID   string `json:"run_id"`
	RepoURL string `json:"repo_url"`
	HeadRef string `json:"head_ref"`
	// BaseRef is the ref the pipeline diffs against to decide which scopes are
	// impacted. Empty means the engine's own default, which assumes a full
	// clone; callers targeting a shallow one SHOULD pass an explicit ref.
	BaseRef string `json:"base_ref"`
	// Task names .sklp/pipelines/<Task>.yaml inside the cloned repo.
	Task string `json:"task"`
	// Secrets are surfaced to the pipeline as environment variables. They are
	// never logged and never persisted on the node.
	Secrets map[string]string `json:"secrets,omitempty"`
	// Provider carries the LLM credentials the pipeline's diagnose step uses.
	// An empty APIKey is allowed: the engine degrades instead of failing.
	Provider Provider `json:"provider,omitempty"`
}

// Provider mirrors the SKLP_* env names the engine already understands.
type Provider struct {
	Kind    string `json:"kind,omitempty"`
	Model   string `json:"model,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
	APIKey  string `json:"api_key,omitempty"`
}

// Frame is one NDJSON line of the /v1/run response stream.
//
// Kind is one of:
//
//	"queued"  — the node holds another run; Text is how many are ahead
//	"started" — the pipeline is executing now; Text is the node's name
//	"log"     — Text is a chunk of the pipeline's human-readable output
//	"report"  — Text is the final JSON report; always the last frame of a run
//	"error"   — Text is why the run could not complete; also terminal
type Frame struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

const (
	FrameQueued  = "queued"
	FrameStarted = "started"
	FrameLog     = "log"
	FrameReport  = "report"
	FrameError   = "error"
)

// HealthResponse is the GET /v1/health body. Running counts the runs the
// node holds, queued ones included, so a dispatcher with several nodes can
// send the next run to an idle one.
type HealthResponse struct {
	Version string `json:"version"`
	Ready   bool   `json:"ready"`
	Running int    `json:"running"`
}

// RunStatusResponse is the GET /v1/run/{id} body.
//
// Status is one of:
//
//	"running" — still executing; Log holds everything captured so far
//	"report"  — finished; Report holds the final JSON, same payload the
//	            "report" stream frame would have carried
//	"error"   — finished without a report; Error holds why
type RunStatusResponse struct {
	Status string `json:"status"`
	Log    string `json:"log"`
	Report string `json:"report,omitempty"`
	Error  string `json:"error,omitempty"`
}

// CacheCleanRequest is the POST /v1/cache/clean body. Mirrors the `sklp
// clean` CLI flags (clean_linux.go's cleanOpts) minus DryRun/Force: this
// route always applies (no dry-run mode — the UI button confirms before
// calling), and never prompts (there is no TTY to prompt on). The handler
// also purges the skt toolchain cache (`skt clean --all`) — sklp clean never
// touches it, yet a poisoned tool marker breaks runs the same way.
type CacheCleanRequest struct {
	Images bool `json:"images,omitempty"`
	Deep   bool `json:"deep,omitempty"`
}

// CacheCleanResponse is the POST /v1/cache/clean body: the same human-readable
// lines `sklp clean` prints to stdout, plus whether every step succeeded.
type CacheCleanResponse struct {
	OK     bool     `json:"ok"`
	Output []string `json:"output"`
}
